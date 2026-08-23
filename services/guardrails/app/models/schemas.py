"""Pydantic v2 models at the service boundary.

Every request and response the guardrails service speaks is defined here. Nothing else in the
service parses a dict: the boundary is typed, so `mypy --strict` can prove that a detector
cannot return a shape the gateway will not understand.
"""

from __future__ import annotations

from enum import StrEnum
from typing import Annotated

from pydantic import BaseModel, ConfigDict, Field


class Severity(StrEnum):
    """How serious a finding is.

    Severity drives the default action, but never overrides an explicit policy: an operator who
    has decided that emails are redacted rather than blocked outranks the detector's opinion.
    """

    LOW = "low"
    MEDIUM = "medium"
    HIGH = "high"
    CRITICAL = "critical"


class Action(StrEnum):
    """What the gateway should do about a finding."""

    ALLOW = "allow"
    REDACT = "redact"
    BLOCK = "block"


class OWASPCategory(StrEnum):
    """The OWASP Top 10 for LLM Applications categories this service detects.

    Mapping every finding to a published taxonomy is what makes the output auditable: a security
    reviewer can ask "what do you do about LLM01" and get an answer, rather than a list of
    bespoke rule names. The full mapping, including the categories this service deliberately
    does not address, is in docs/owasp-llm-mapping.md.
    """

    LLM01_PROMPT_INJECTION = "LLM01"
    LLM02_INSECURE_OUTPUT = "LLM02"
    LLM04_MODEL_DOS = "LLM04"
    LLM06_SENSITIVE_INFO = "LLM06"
    LLM08_EXCESSIVE_AGENCY = "LLM08"


class Finding(BaseModel):
    """One detection."""

    model_config = ConfigDict(frozen=True)

    detector: str = Field(description="which detector fired, e.g. 'pii' or 'prompt_injection'")
    category: str = Field(description="the specific rule, e.g. 'email' or 'instruction_override'")
    owasp: OWASPCategory
    severity: Severity
    action: Action
    start: int = Field(ge=0, description="start offset of the match in the original text")
    end: int = Field(ge=0, description="end offset, exclusive")
    # The matched text is deliberately NOT included. Returning it would copy the secret or the
    # PII into the gateway's logs and into the analytics event, which is the exact leak this
    # service exists to prevent.
    excerpt_hash: str = Field(
        description="sha256 of the matched span, so two reports of the same secret can be "
        "correlated without the secret itself ever being transmitted"
    )
    confidence: float = Field(ge=0.0, le=1.0)


class ScreenRequest(BaseModel):
    """POST /v1/screen/input and /v1/screen/output."""

    model_config = ConfigDict(extra="forbid")

    tenant_id: str = Field(min_length=1, max_length=128)
    text: str = Field(max_length=1_000_000)
    policy: str | None = Field(
        default=None,
        description="named policy override; falls back to the default policy when unset",
    )
    # Kept so a caller can correlate a screening decision with the request that caused it.
    request_id: str | None = Field(default=None, max_length=128)


class ScreenResponse(BaseModel):
    """The verdict."""

    allowed: bool
    redacted_text: str = Field(
        description="the text with every redactable finding replaced. Equal to the input when "
        "nothing was redacted, so the caller can use it unconditionally."
    )
    findings: list[Finding]
    latency_ms: float
    policy: str
    # True when the text was truncated before scanning; see MAX_SCAN_CHARS.
    truncated: bool = False


class PolicyRule(BaseModel):
    """How one detector is configured under a policy."""

    detector: str
    enabled: bool
    action: Action
    min_severity: Severity


class PolicyView(BaseModel):
    """A policy as exposed by GET /v1/policies."""

    name: str
    description: str
    rules: list[PolicyRule]


class PoliciesResponse(BaseModel):
    """GET /v1/policies."""

    default_policy: str
    policies: list[PolicyView]
    owasp_coverage: dict[str, list[str]] = Field(
        description="OWASP category -> the detectors that address it"
    )


class HealthResponse(BaseModel):
    """GET /healthz."""

    status: str
    version: str
    detectors: list[str]
    toxicity_enabled: bool


Text = Annotated[str, Field(max_length=1_000_000)]
