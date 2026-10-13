"""Task classification: rules-first request typing for routing.

Mirrors the Go classifier's taxonomy so worker analysis and gateway routing
agree: chat, coding, summarization, extraction, reasoning, translation,
tool-use, structured output, long-context, high-priority interactive,
batch/offline.
"""

from __future__ import annotations

import re
from collections.abc import Mapping
from typing import Any

TASKS = (
    "chat",
    "coding",
    "summarization",
    "extraction",
    "reasoning",
    "translation",
    "tool-use",
    "structured_output",
    "long_context",
    "high_priority_interactive",
    "batch_offline",
)

_SUMMARIZE = re.compile(r"summariz|tl;dr|tldr|gist", re.IGNORECASE)
_TRANSLATE = re.compile(r"translat", re.IGNORECASE)
_EXTRACT = re.compile(r"\bextract\b|pull out|list all|find all", re.IGNORECASE)
_REASON = re.compile(r"prove|step by step|chain of thought|theorem|\bqed\b", re.IGNORECASE)
_CODE_FENCE = re.compile(r"```")
_CODE_KW = re.compile(r"\b(def|class|func|function|import|package|return|const|let|SELECT)\b")


def _messages_text(request: Mapping[str, Any]) -> str:
    parts: list[str] = []
    messages = request.get("messages", [])
    if isinstance(messages, (list, tuple)):
        for m in messages:
            if not isinstance(m, Mapping):
                continue
            content = m.get("content")
            if isinstance(content, str):
                parts.append(content)
            elif isinstance(content, (list, tuple)):
                for part in content:
                    if isinstance(part, Mapping) and isinstance(part.get("text"), str):
                        parts.append(part["text"])
    return "\n".join(parts)


def classify_task(
    request: Mapping[str, Any],
    prompt_tokens: int = 0,
    *,
    long_context_tokens: int = 32000,
    batch_tokens: int = 64000,
) -> dict[str, Any]:
    """Classify a request, returning task, confidence and signals."""
    signals: list[str] = []
    secondary: list[str] = []
    text = _messages_text(request)
    lower = text.lower()

    tools = request.get("tools")
    has_tools = isinstance(tools, (list, tuple)) and len(tools) > 0
    response_format = request.get("response_format")
    wants_structured = isinstance(response_format, Mapping) and str(response_format.get("type", "")) in {
        "json_object",
        "json_schema",
    }
    streaming = bool(request.get("stream"))

    if has_tools:
        task, conf = "tool-use", 1.0
        signals.append("tools_present")
    elif wants_structured:
        task, conf = "structured_output", 1.0
        signals.append("response_format_structured")
    else:
        task, conf = "chat", 0.6
        signals.append("default_chat")

    if prompt_tokens >= long_context_tokens:
        if task != "chat":
            secondary.append(task)
        task, conf = "long_context", 0.9
        signals.append("long_prompt")
    elif not streaming and prompt_tokens >= batch_tokens:
        if task != "chat":
            secondary.append(task)
        task, conf = "batch_offline", 0.8
        signals.append("batch_candidate")

    if task == "chat":
        if _SUMMARIZE.search(lower):
            task, conf = "summarization", 0.8
            signals.append("summarize_keywords")
        elif _TRANSLATE.search(lower):
            task, conf = "translation", 0.8
            signals.append("translate_keywords")
        elif _EXTRACT.search(lower):
            task, conf = "extraction", 0.8
            signals.append("extract_keywords")
        elif _REASON.search(lower):
            task, conf = "reasoning", 0.8
            signals.append("reasoning_keywords")
        elif _CODE_FENCE.search(text) or _CODE_KW.search(text):
            task, conf = "coding", 0.8
            signals.append("code_markers")

    if streaming and prompt_tokens < 4000 and task == "chat":
        secondary.append("high_priority_interactive")
        signals.append("streaming_short")

    return {
        "task": task,
        "secondary": secondary,
        "confidence": conf,
        "signals": signals,
        "prompt_tokens": prompt_tokens,
    }
