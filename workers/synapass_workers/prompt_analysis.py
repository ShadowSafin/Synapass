"""Static prompt analysis.

Two questions are answered here, both without calling a model:

1. **What will this request need?** Token size, whether it carries an image or a
   tool call, whether it demands structured output. Routing uses that to avoid
   spending a frontier model on a one-line question and to avoid sending a
   vision request to a text-only model.

2. **What is in it?** Code, non-ASCII content, and patterns that look like
   credentials or personal data. Those are reported as flags so a deployment can
   decide what to do; the analyser never blocks anything itself, because a
   heuristic that silently refuses traffic is worse than one that surfaces it.

Everything here is deliberately dull and explainable. A single opaque "quality
score" would be impossible to debug when routing goes somewhere surprising, which
is exactly the situation an operator needs to understand.
"""

from __future__ import annotations

import re
import unicodedata
from collections.abc import Iterable, Mapping, Sequence
from dataclasses import dataclass
from typing import Any

from . import ENGINE_VERSION

# The token estimator matches the Go control plane's heuristic so a prompt
# analysed here and a prompt routed there produce the same size. It is a
# character-based estimate because the gateway must not depend on a tokeniser
# per provider: a provider change would then change routing behaviour.
CHARS_PER_TOKEN = 3.6

# A floor, because very short strings under-estimate badly with a character
# ratio: "hi" is not zero tokens.
MIN_TOKENS_PER_MESSAGE = 2
# Per-message framing overhead charged by every chat template.
MESSAGE_FRAMING_TOKENS = 4
# Per-tool declaration overhead.
TOOL_FRAMING_TOKENS = 12

# Capability names are the same strings the Go domain uses, so an analysis result
# can be compared against a model registry entry without a translation table.
CAP_CHAT = "chat"
CAP_STREAMING = "streaming"
CAP_TOOLS = "tools"
CAP_PARALLEL_TOOLS = "parallel_tools"
CAP_VISION = "vision"
CAP_JSON_MODE = "json_mode"
CAP_JSON_SCHEMA = "json_schema"
CAP_LONG_CONTEXT = "long_context"
CAP_REASONING = "reasoning"

# Long-context threshold. It matches the registry's notion of "large window"
# closely enough for a hint, which is all this is: the router still checks the
# model's real context window before accepting a candidate.
LONG_CONTEXT_TOKENS = 32_000

_CODE_FENCE = re.compile(r"```")
# A line that looks like code: a keyword followed by punctuation, or an obvious
# assignment/declaration. Deliberately conservative to avoid flagging prose.
_CODE_KEYWORDS = re.compile(
    r"\b(def|class|func|function|import|from|package|return|const|var|let|"
    r"public|private|void|struct|interface|SELECT|INSERT|UPDATE|DELETE|"
    r"#!/|\$\(|=>|::|->)\b"
)
_EMAIL = re.compile(r"\b[\w.+-]+@[\w-]+\.[\w.-]+\b")
_PHONE = re.compile(r"(?<!\w)(?:\+?\d[\d\s().-]{7,}\d)(?!\w)")
# Credential-shaped strings: a long unbroken run that mixes cases and digits, or a
# known prefix. This catches the common paste-a-key mistake, not every secret.
_API_KEY = re.compile(r"\b(?:sk|pk|ghp|gho|github_pat|xox[baprs]|AKIA|AIza)[_-]?[A-Za-z0-9_-]{16,}\b")
_BEARER = re.compile(r"\bBearer\s+[A-Za-z0-9._\-]{20,}\b", re.IGNORECASE)
_SSN = re.compile(r"\b\d{3}-\d{2}-\d{4}\b")
_CARD_CANDIDATE = re.compile(r"(?<!\d)(?:\d[ -]?){13,19}(?!\d)")

# Order matters for stable output: report_patterns sorts, so this is just the
# canonical list of what the analyser looks for.
PROTECTIVE_PATTERNS: tuple[str, ...] = (
    "email",
    "phone",
    "credit_card",
    "government_id",
    "api_key",
    "bearer_token",
)


def estimate_tokens(text: str) -> int:
    """Estimate the token count of a string."""
    if not text:
        return 0
    stripped = text.strip()
    if not stripped:
        return 0
    return max(1, int(len(stripped) / CHARS_PER_TOKEN))


def luhn_valid(digits: str) -> bool:
    """Validate a digit string with the Luhn checksum.

    Card numbers are the one sensitive pattern common enough to produce false
    positives from ordinary numeric text, so a checksum is applied before the flag
    is raised. Flagging every 16-digit number would train operators to ignore the
    flag entirely.
    """
    compact = re.sub(r"\D", "", digits)
    if len(compact) < 13 or len(compact) > 19:
        return False
    total = 0
    double = False
    for char in reversed(compact):
        value = int(char)
        if double:
            value *= 2
            if value > 9:
                value -= 9
        total += value
        double = not double
    return total % 10 == 0


def message_text(content: Any) -> str:
    """Flatten a message ``content`` field, which may be a string or a part list."""
    if isinstance(content, str):
        return content
    if isinstance(content, Sequence) and not isinstance(content, (str, bytes)):
        chunks: list[str] = []
        for part in content:
            if isinstance(part, Mapping):
                text = part.get("text")
                if isinstance(text, str):
                    chunks.append(text)
            elif isinstance(part, str):
                chunks.append(part)
        return "\n".join(chunks)
    return ""


def content_has_image(content: Any) -> bool:
    """Report whether a content part list carries an image."""
    if not isinstance(content, Sequence) or isinstance(content, (str, bytes)):
        return False
    for part in content:
        if not isinstance(part, Mapping):
            continue
        if str(part.get("type", "")) in {"image_url", "image", "input_image"}:
            return True
    return False


def messages_text(messages: Iterable[Mapping[str, Any]]) -> str:
    """Concatenate every message's text for estimation and analysis."""
    return "\n".join(message_text(message.get("content")) for message in messages)


def estimate_request_tokens(request: Mapping[str, Any]) -> int:
    """Estimate the prompt size of a chat completion request.

    Framing overhead is added per message and per tool because it is real: a
    template wraps every turn in role markers and separators, and a tool
    declaration is several hundred characters before its schema. Ignoring it
    under-estimates by enough to overflow a small context window.
    """
    messages = request.get("messages")
    total = 0
    if isinstance(messages, Sequence) and not isinstance(messages, (str, bytes)):
        for message in messages:
            if not isinstance(message, Mapping):
                continue
            text_tokens = estimate_tokens(message_text(message.get("content")))
            total += max(text_tokens, MIN_TOKENS_PER_MESSAGE) + MESSAGE_FRAMING_TOKENS
            # A tool-call payload travels as its own field and is not counted by
            # the content estimate.
            tool_calls = message.get("tool_calls")
            if isinstance(tool_calls, Sequence) and not isinstance(tool_calls, (str, bytes)):
                for call in tool_calls:
                    if isinstance(call, Mapping):
                        function = call.get("function")
                        if isinstance(function, Mapping):
                            total += estimate_tokens(
                                f"{function.get('name', '')}{function.get('arguments', '')}"
                            )

    tools = request.get("tools")
    if isinstance(tools, Sequence) and not isinstance(tools, (str, bytes)):
        for tool in tools:
            if isinstance(tool, Mapping):
                function = tool.get("function")
                if isinstance(function, Mapping):
                    total += (
                        estimate_tokens(f"{function.get('name', '')}{function.get('description', '')}")
                        + TOOL_FRAMING_TOKENS
                    )

    return total


def contains_code(text: str) -> bool:
    """Heuristically detect source code in prose."""
    if not text:
        return False
    if _CODE_FENCE.search(text):
        return True
    return bool(_CODE_KEYWORDS.search(text))


def wants_json(request: Mapping[str, Any]) -> bool:
    """Report whether the request asks for structured output."""
    response_format = request.get("response_format")
    if isinstance(response_format, Mapping):
        kind = str(response_format.get("type", ""))
        if kind in {"json_object", "json_schema"}:
            return True
    return False


def wants_json_schema(request: Mapping[str, Any]) -> bool:
    """Report whether the request supplies an explicit JSON schema."""
    response_format = request.get("response_format")
    if isinstance(response_format, Mapping):
        return str(response_format.get("type", "")) == "json_schema"
    return False


def non_ascii_ratio(text: str) -> float:
    """Fraction of characters outside the ASCII range, rounded to three places.

    This is a script profile, not language detection, and it is named accordingly:
    a high ratio says the text is not English-only, which is enough to know that a
    text model choice matters, without pretending to identify the language.
    """
    if not text:
        return 0.0
    normalised = unicodedata.normalize("NFKC", text)
    non_ascii = sum(1 for char in normalised if ord(char) > 127)
    return round(non_ascii / len(normalised), 3)


def sensitive_patterns(text: str) -> tuple[str, ...]:
    """Return the names of protective patterns found in the text, sorted."""
    if not text:
        return ()
    found: set[str] = set()
    if _EMAIL.search(text):
        found.add("email")
    if _API_KEY.search(text):
        found.add("api_key")
    if _BEARER.search(text):
        found.add("bearer_token")
    if _SSN.search(text):
        found.add("government_id")
    if _PHONE.search(text):
        found.add("phone")
    if any(luhn_valid(match.group(0)) for match in _CARD_CANDIDATE.finditer(text)):
        found.add("credit_card")
    return tuple(sorted(found))


def detect_capabilities(request: Mapping[str, Any]) -> tuple[str, ...]:
    """Derive the capability set a request requires.

    Only capabilities that are actually expressed in the payload are reported.
    Streaming is included because it is a property of the request itself; tools and
    vision are included because the request cannot be served without them.
    """
    capabilities: set[str] = {CAP_CHAT}

    if _as_bool(request.get("stream")):
        capabilities.add(CAP_STREAMING)

    tools = request.get("tools")
    if isinstance(tools, Sequence) and not isinstance(tools, (str, bytes)) and len(tools) > 0:
        capabilities.add(CAP_TOOLS)
        if _as_bool(request.get("parallel_tool_calls")):
            capabilities.add(CAP_PARALLEL_TOOLS)

    messages = request.get("messages")
    if isinstance(messages, Sequence) and not isinstance(messages, (str, bytes)):
        for message in messages:
            if isinstance(message, Mapping) and content_has_image(message.get("content")):
                capabilities.add(CAP_VISION)
                break

    if wants_json_schema(request):
        capabilities.add(CAP_JSON_SCHEMA)
        capabilities.add(CAP_JSON_MODE)
    elif wants_json(request):
        capabilities.add(CAP_JSON_MODE)

    if _as_bool(request.get("seed")) or request.get("seed") == 0:
        capabilities.add("seed")

    effort = str(request.get("reasoning_effort", "")).strip().lower()
    if effort and effort != "none":
        capabilities.add(CAP_REASONING)

    return tuple(sorted(capabilities))


def classify_complexity(
    prompt_tokens: int,
    *,
    has_tools: bool,
    has_code: bool,
    has_image: bool,
    message_count: int,
    reasoning: bool,
    long_context_tokens: int = LONG_CONTEXT_TOKENS,
) -> str:
    """Bucket a request into ``trivial``, ``simple``, ``moderate`` or ``complex``.

    The buckets are ordered, and the highest applicable one wins, so the rules read
    as a priority list rather than as an opaque score. Every bucket boundary is a
    named constant so an operator can see why a request landed where it did.
    """
    if prompt_tokens >= long_context_tokens or has_tools or reasoning:
        return "complex"
    if has_image or has_code or message_count >= 6:
        return "moderate"
    if prompt_tokens <= 200 and message_count <= 2:
        return "trivial"
    return "simple"


# Quality tiers mirror the registry's 1..5 scale, where 5 is the strongest model.
COMPLEXITY_TIERS: Mapping[str, int] = {
    "trivial": 1,
    "simple": 2,
    "moderate": 3,
    "complex": 5,
}

# The strategy suggestion follows the tier. It is a suggestion: a stored policy
# always wins, and the analysis is only consulted when no policy pins a strategy.
COMPLEXITY_STRATEGIES: Mapping[str, str] = {
    "trivial": "lowest_cost",
    "simple": "lowest_cost",
    "moderate": "priority",
    "complex": "highest_quality",
}


@dataclass(frozen=True)
class _Findings:
    text: str
    prompt_tokens: int
    message_count: int
    capabilities: tuple[str, ...]
    has_code: bool
    wants_json: bool
    non_ascii: float
    sensitive: tuple[str, ...]


def _gather(request: Mapping[str, Any]) -> _Findings:
    messages: list[Mapping[str, Any]] = []
    raw_messages = request.get("messages")
    if isinstance(raw_messages, Sequence) and not isinstance(raw_messages, (str, bytes)):
        messages = [m for m in raw_messages if isinstance(m, Mapping)]

    text = messages_text(messages)
    capabilities = detect_capabilities(request)
    return _Findings(
        text=text,
        prompt_tokens=estimate_request_tokens(request),
        message_count=len(messages),
        capabilities=capabilities,
        has_code=contains_code(text),
        wants_json=wants_json(request),
        non_ascii=non_ascii_ratio(text),
        sensitive=sensitive_patterns(text),
    )


def _flags(findings: _Findings, max_tokens: int) -> tuple[str, ...]:
    """Derive operator-facing flags from the findings."""
    flags: set[str] = set()
    if findings.sensitive:
        flags.add("contains_sensitive_data")
    if "api_key" in findings.sensitive or "bearer_token" in findings.sensitive:
        flags.add("possible_credential_leak")
    if max_tokens > 0 and findings.prompt_tokens > max_tokens:
        flags.add("prompt_exceeds_analysis_limit")
    if findings.non_ascii >= 0.3:
        flags.add("predominantly_non_ascii")
    if findings.has_code:
        flags.add("contains_code")
    return tuple(sorted(flags))


def analyze_request(
    request: Mapping[str, Any],
    *,
    request_id: str = "",
    max_tokens: int = 0,
    long_context_tokens: int = LONG_CONTEXT_TOKENS,
) -> dict[str, Any]:
    """Analyse a chat completion request and return a serialisable result."""
    findings = _gather(request)

    complexity = classify_complexity(
        findings.prompt_tokens,
        has_tools=CAP_TOOLS in findings.capabilities,
        has_code=findings.has_code,
        has_image=CAP_VISION in findings.capabilities,
        message_count=findings.message_count,
        reasoning=CAP_REASONING in findings.capabilities,
        long_context_tokens=long_context_tokens,
    )

    capabilities = list(findings.capabilities)
    # A prompt large enough to need a long window is a capability requirement, not
    # a preference: routing to a 8k-window model would simply fail.
    if findings.prompt_tokens >= long_context_tokens and CAP_LONG_CONTEXT not in capabilities:
        capabilities.append(CAP_LONG_CONTEXT)
    capabilities.sort()

    return {
        "request_id": request_id,
        "prompt_tokens": findings.prompt_tokens,
        "completion_tokens_estimate": estimate_completion_tokens(request),
        "message_count": findings.message_count,
        "capabilities": capabilities,
        "complexity": complexity,
        "suggested_quality_tier": COMPLEXITY_TIERS.get(complexity, 2),
        "suggested_strategy": COMPLEXITY_STRATEGIES.get(complexity, "priority"),
        "contains_code": findings.has_code,
        "wants_json": findings.wants_json,
        "non_ascii_ratio": findings.non_ascii,
        "sensitive_patterns": list(findings.sensitive),
        "flags": list(_flags(findings, max_tokens)),
        "engine_version": ENGINE_VERSION,
    }


def estimate_completion_tokens(request: Mapping[str, Any]) -> int:
    """Estimate the completion allowance the client asked for.

    ``max_completion_tokens`` wins over ``max_tokens`` because OpenAI prefers it
    for newer models and a client that sends both means the newer field.
    """
    for key in ("max_completion_tokens", "max_tokens"):
        value = request.get(key)
        if isinstance(value, (int, float)) and not isinstance(value, bool) and value > 0:
            return int(value)
    return 0


def analyze_text(
    text: str,
    *,
    request_id: str = "",
    max_tokens: int = 0,
) -> dict[str, Any]:
    """Analyse a bare prompt string."""
    tokens = estimate_tokens(text)
    sensitive = sensitive_patterns(text)
    has_code = contains_code(text)
    findings = _Findings(
        text=text,
        prompt_tokens=tokens,
        message_count=1,
        capabilities=(CAP_CHAT,),
        has_code=has_code,
        wants_json=False,
        non_ascii=non_ascii_ratio(text),
        sensitive=sensitive,
    )
    complexity = classify_complexity(
        tokens,
        has_tools=False,
        has_code=has_code,
        has_image=False,
        message_count=1,
        reasoning=False,
    )
    return {
        "request_id": request_id,
        "prompt_tokens": tokens,
        "completion_tokens_estimate": 0,
        "message_count": 1,
        "capabilities": [CAP_CHAT],
        "complexity": complexity,
        "suggested_quality_tier": COMPLEXITY_TIERS.get(complexity, 2),
        "suggested_strategy": COMPLEXITY_STRATEGIES.get(complexity, "priority"),
        "contains_code": has_code,
        "wants_json": False,
        "non_ascii_ratio": findings.non_ascii,
        "sensitive_patterns": list(sensitive),
        "flags": list(_flags(findings, max_tokens)),
        "engine_version": ENGINE_VERSION,
    }


def _as_bool(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        return value.strip().lower() in {"1", "true", "yes", "on"}
    if isinstance(value, (int, float)):
        return value != 0
    return False
