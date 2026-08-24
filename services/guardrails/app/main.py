"""LLMRouter guardrails service.

Screens prompts before they reach a provider, and optionally screens completions on the way
back. The gateway calls this on every request, which sets the design constraints:

  - It must be fast. The performance budget is a p99 under 12 ms for the whole input pipeline,
    enforced by benchmarks/guardrails/bench_guardrails.py and gated in CI.
  - It must not fail. Detectors are pure functions over strings with no I/O; the scan is
    wrapped so that a bug in one detector degrades that detector rather than the request.
  - It must not leak. Findings carry offsets and a hash of the matched span, never the span
    itself, so a secret detected here is not then copied into the gateway's logs.
"""

from __future__ import annotations

import logging
import os
import time
from typing import Annotated

from fastapi import Depends, FastAPI
from fastapi.responses import JSONResponse

from app.detectors.base import Detector, redact
from app.detectors.pii import PIIDetector
from app.detectors.prompt_injection import PromptInjectionDetector
from app.detectors.secrets import SecretsDetector
from app.detectors.toxicity import ToxicityDetector
from app.models.schemas import (
    Finding,
    HealthResponse,
    PoliciesResponse,
    ScreenRequest,
    ScreenResponse,
)
from app.policies.owasp import DEFAULT_POLICY, OWASP_COVERAGE, POLICIES, resolve

VERSION = "1.0.0"

LOG = logging.getLogger("guardrails")

#: Beyond this many characters the scan is truncated.
#:
#: Regex scanning is linear, but a one-megabyte prompt would still blow the latency budget, and
#: the interesting content in a long prompt is overwhelmingly at the start (the instruction) and
#: the end (the payload). Truncation is reported in the response so a caller is never misled
#: into thinking a long document was fully screened.
MAX_SCAN_CHARS = 24_000


def build_detectors(toxicity_enabled: bool) -> list[Detector]:
    """Construct the detector chain.

    Order is irrelevant to correctness -- findings are merged by span afterwards -- but it is
    kept cheapest-first so that the common case of a clean prompt exits the loop having done the
    least work.
    """
    detectors: list[Detector] = [SecretsDetector(), PIIDetector(), PromptInjectionDetector()]
    if toxicity_enabled:
        detectors.append(ToxicityDetector())
    return detectors


class Screener:
    """Runs the detector chain and applies a policy."""

    def __init__(self, detectors: list[Detector]) -> None:
        self.detectors = detectors

    @property
    def names(self) -> list[str]:
        return [d.name for d in self.detectors]

    def screen(self, text: str, policy_name: str | None) -> ScreenResponse:
        started = time.perf_counter()

        truncated = len(text) > MAX_SCAN_CHARS
        scanned = text[:MAX_SCAN_CHARS] if truncated else text

        findings: list[Finding] = []
        for detector in self.detectors:
            try:
                findings.extend(detector.scan(scanned))
            except Exception:  # noqa: BLE001 - deliberate: one bad detector must not fail the request
                # A detector that throws is a bug, but failing the whole request over it would
                # turn a bug into an outage. It is logged loudly and the rest still run.
                LOG.exception("detector %s raised; continuing without it", detector.name)

        policy = resolve(policy_name)
        allowed, kept = policy.apply(findings)

        return ScreenResponse(
            allowed=allowed,
            # Redaction runs against the ORIGINAL text so that offsets stay valid and a
            # truncated tail is preserved rather than silently dropped from the prompt.
            redacted_text=redact(text, kept) if allowed else text,
            findings=kept,
            latency_ms=round((time.perf_counter() - started) * 1000, 4),
            policy=policy.name,
            truncated=truncated,
        )


TOXICITY_ENABLED = os.getenv("GUARDRAILS_TOXICITY_ENABLED", "false").strip().lower() in ("1", "true", "yes")

_screener = Screener(build_detectors(TOXICITY_ENABLED))


def get_screener() -> Screener:
    """FastAPI dependency: the process-wide screener.

    Injected rather than imported so a test can substitute a screener with a different detector
    chain without touching module state.
    """
    return _screener


app = FastAPI(
    title="LLMRouter guardrails",
    version=VERSION,
    docs_url="/docs",
    description=__doc__,
)


@app.get("/healthz", response_model=HealthResponse)
def healthz(screener: Annotated[Screener, Depends(get_screener)]) -> HealthResponse:
    """Liveness, plus which detectors are actually running."""
    return HealthResponse(
        status="ok",
        version=VERSION,
        detectors=screener.names,
        toxicity_enabled=TOXICITY_ENABLED,
    )


@app.post("/v1/screen/input", response_model=ScreenResponse)
def screen_input(
    body: ScreenRequest,
    screener: Annotated[Screener, Depends(get_screener)],
) -> ScreenResponse:
    """Screen a prompt before it reaches a provider."""
    return screener.screen(body.text, body.policy)


@app.post("/v1/screen/output", response_model=ScreenResponse)
def screen_output(
    body: ScreenRequest,
    screener: Annotated[Screener, Depends(get_screener)],
) -> ScreenResponse:
    """Screen a completion on the way back to the caller.

    The same detectors, because the risks are symmetric: a model can echo back the PII it was
    given (LLM06) or emit markup that a downstream renderer will execute (LLM02). It is a
    separate endpoint rather than a flag so that output screening can be enabled independently
    -- it doubles the latency cost, and not every deployment wants to pay it.
    """
    return screener.screen(body.text, body.policy)


@app.get("/v1/policies", response_model=PoliciesResponse)
def policies() -> PoliciesResponse:
    """List the configured policies and the OWASP categories they cover."""
    return PoliciesResponse(
        default_policy=DEFAULT_POLICY,
        policies=[p.view() for p in POLICIES.values()],
        owasp_coverage=OWASP_COVERAGE,
    )


@app.exception_handler(Exception)
async def unhandled(_: object, exc: Exception) -> JSONResponse:
    """Never leak an internal error, and never leak the text that caused it."""
    LOG.exception("unhandled error in the guardrails service", exc_info=exc)
    return JSONResponse(status_code=500, content={"detail": "internal error"})
