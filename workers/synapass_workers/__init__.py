"""Synapass intelligence workers.

These services hold the parts of Synapass that benefit from Python: offline
scoring, prompt analysis, evaluation and replay processing. The synchronous
request path stays in Go; nothing here is ever on the critical path of a
completion.

# Layering

The package is deliberately split so the interesting logic is importable without
any third-party dependency:

* Pure modules -- :mod:`scoring`, :mod:`prompt_analysis`, :mod:`telemetry`,
  :mod:`models`, :mod:`config` -- import only the standard library. They are the
  parts that encode judgement, and they are fully unit tested.
* Transport modules -- :mod:`bus`, :mod:`metrics`, and the HTTP provider client --
  import ``nats-py``, ``prometheus_client`` and ``httpx`` lazily, inside the
  functions that need them.

That split is what lets ``python -m unittest`` run the whole test suite on a bare
interpreter while production still gets JetStream durability and Prometheus
metrics.
"""

from __future__ import annotations

__all__ = ["ENGINE_VERSION", "__version__"]

__version__ = "1.0.0"

# ENGINE_VERSION identifies the scoring engine revision in every persisted result.
# It exists so a score computed by an older build stays interpretable: when the
# scoring rules change, an operator can tell which rows were produced by which
# rules instead of silently comparing incomparable numbers.
ENGINE_VERSION = "scoring-v1"
