# Synapass intelligence workers.
#
# Multi-stage so the runtime image carries no compiler, no build tooling and no
# pip cache. The core scoring logic is dependency-free, so the runtime image only
# needs the transport libraries the deployed worker actually uses.

# ---------------------------------------------------------------------------
# Build stage: install dependencies into a virtual environment.
# ---------------------------------------------------------------------------
FROM python:3.12-slim AS builder

ENV PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_NO_CACHE_DIR=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /build

# A virtual environment rather than a system install: copying one directory is
# unambiguous, and it keeps the runtime stage from inheriting pip's state.
RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

# Requirements are copied first so a code change does not invalidate the
# dependency layer. This is the difference between a two-second and a
# two-minute rebuild.
COPY requirements.txt ./
RUN pip install --upgrade pip && pip install -r requirements.txt

# ---------------------------------------------------------------------------
# Runtime stage: only the venv, the source and an unprivileged user.
# ---------------------------------------------------------------------------
FROM python:3.12-slim AS runtime

# PYTHONUNBUFFERED is required for logs to appear: without it stdout is buffered
# and a container that crashes shows nothing at all, which is the worst possible
# failure mode for a service that only does background work.
ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONFAULTHANDLER=1 \
    PATH="/opt/venv/bin:$PATH" \
    SYNAPASS_WORKER_METRICS_ADDR="0.0.0.0:9101" \
    SYNAPASS_WORKER_LOG_FORMAT="json"

COPY --from=builder /opt/venv /opt/venv

RUN useradd --create-home --shell /usr/sbin/nologin --uid 10001 synapass
WORKDIR /app
COPY --chown=synapass:synapass synapass_workers ./synapass_workers
COPY --chown=synapass:synapass pyproject.toml README.md ./

USER synapass

# The metrics port doubles as the liveness endpoint: a worker whose loop has died
# stops serving it, which is the only externally visible symptom this process has.
EXPOSE 9101

# urllib from the standard library, because the slim image ships no curl/wget.
# Compose also declares this probe so `docker compose ps` reports health even
# when the image is overridden.
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
  CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:9101/metrics', timeout=4)"

# Tini-style signal forwarding is unnecessary here because the entrypoint is the
# interpreter itself (PID 1 is python), but exec form is still used so SIGTERM
# reaches it directly.
ENTRYPOINT ["python", "-m", "synapass_workers.cli"]
CMD ["serve"]
