"""Policies: turning findings into a verdict, and mapping them onto OWASP.

A detector says what it saw. A policy decides what to do about it. Keeping those separate is
what lets one deployment redact emails while another blocks them, without touching detector
code -- and it is what makes the OWASP mapping a property of the service rather than a claim in
a README.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from app.models.schemas import Action, Finding, OWASPCategory, PolicyRule, PolicyView, Severity

SEVERITY_ORDER: dict[Severity, int] = {
    Severity.LOW: 0,
    Severity.MEDIUM: 1,
    Severity.HIGH: 2,
    Severity.CRITICAL: 3,
}


@dataclass(frozen=True)
class DetectorPolicy:
    """How one detector behaves under a policy."""

    enabled: bool = True
    # None means "use whatever the detector proposed", which is the sensible default: the
    # detector knows that an SSN is worse than an IP address.
    action: Action | None = None
    min_severity: Severity = Severity.LOW


@dataclass(frozen=True)
class Policy:
    """A named screening configuration."""

    name: str
    description: str
    detectors: dict[str, DetectorPolicy] = field(default_factory=dict)

    def rule_for(self, detector: str) -> DetectorPolicy:
        return self.detectors.get(detector, DetectorPolicy())

    def apply(self, findings: list[Finding]) -> tuple[bool, list[Finding]]:
        """Filter findings by policy and decide whether the request is allowed.

        Returns the surviving findings with their policy-resolved actions. A finding below the
        configured severity floor is dropped entirely rather than downgraded, so the caller's
        finding count means "things this policy cares about" rather than "things we noticed".
        """
        kept: list[Finding] = []
        allowed = True

        for f in findings:
            rule = self.rule_for(f.detector)
            if not rule.enabled:
                continue
            if SEVERITY_ORDER[f.severity] < SEVERITY_ORDER[rule.min_severity]:
                continue

            action = rule.action if rule.action is not None else f.action
            resolved = f.model_copy(update={"action": action})
            kept.append(resolved)

            if action is Action.BLOCK:
                allowed = False

        return allowed, kept

    def view(self) -> PolicyView:
        """Render for GET /v1/policies."""
        names = sorted(set(self.detectors) | {"pii", "prompt_injection", "secrets", "toxicity"})
        return PolicyView(
            name=self.name,
            description=self.description,
            rules=[
                PolicyRule(
                    detector=name,
                    enabled=self.rule_for(name).enabled,
                    action=self.rule_for(name).action or Action.REDACT,
                    min_severity=self.rule_for(name).min_severity,
                )
                for name in names
            ],
        )


#: The shipped policies.
#:
#: `balanced` is the default: redact what can be safely rewritten, block what cannot. `strict`
#: is for regulated tenants -- it blocks on PII rather than redacting, because in some contexts
#: the fact that a record was sent at all is the problem. `permissive` exists for internal
#: tooling where the operator has decided the traffic is trusted; it still screens for secrets,
#: because pasting a live credential is a mistake regardless of trust.
POLICIES: dict[str, Policy] = {
    "balanced": Policy(
        name="balanced",
        description="Redact PII, block injections and secrets. The default.",
        detectors={
            "pii": DetectorPolicy(enabled=True, action=Action.REDACT, min_severity=Severity.LOW),
            "prompt_injection": DetectorPolicy(enabled=True, action=None, min_severity=Severity.MEDIUM),
            "secrets": DetectorPolicy(enabled=True, action=Action.BLOCK, min_severity=Severity.HIGH),
            "toxicity": DetectorPolicy(enabled=False),
        },
    ),
    "strict": Policy(
        name="strict",
        description="Block on any PII or injection signal. For regulated tenants.",
        detectors={
            "pii": DetectorPolicy(enabled=True, action=Action.BLOCK, min_severity=Severity.LOW),
            "prompt_injection": DetectorPolicy(enabled=True, action=Action.BLOCK, min_severity=Severity.LOW),
            "secrets": DetectorPolicy(enabled=True, action=Action.BLOCK, min_severity=Severity.LOW),
            "toxicity": DetectorPolicy(enabled=True, action=Action.BLOCK, min_severity=Severity.MEDIUM),
        },
    ),
    "permissive": Policy(
        name="permissive",
        description="Secrets only. For trusted internal tooling.",
        detectors={
            "pii": DetectorPolicy(enabled=False),
            "prompt_injection": DetectorPolicy(enabled=True, action=Action.ALLOW, min_severity=Severity.HIGH),
            "secrets": DetectorPolicy(enabled=True, action=Action.BLOCK, min_severity=Severity.HIGH),
            "toxicity": DetectorPolicy(enabled=False),
        },
    ),
}

DEFAULT_POLICY = "balanced"

#: Which detectors address which OWASP category.
#:
#: Published so the coverage claim is checkable rather than asserted, and so the gaps are
#: visible: LLM03 (training data poisoning), LLM05 (supply chain) and LLM10 (model theft) are
#: not gateway concerns, and LLM07 (insecure plugin design) belongs to whatever calls the
#: gateway. docs/owasp-llm-mapping.md explains each decision.
OWASP_COVERAGE: dict[str, list[str]] = {
    OWASPCategory.LLM01_PROMPT_INJECTION.value: ["prompt_injection"],
    OWASPCategory.LLM02_INSECURE_OUTPUT.value: ["toxicity"],
    OWASPCategory.LLM04_MODEL_DOS.value: ["(gateway: per-tenant token budgets)"],
    OWASPCategory.LLM06_SENSITIVE_INFO.value: ["pii", "secrets"],
    OWASPCategory.LLM08_EXCESSIVE_AGENCY.value: ["prompt_injection:exfiltration"],
}


def resolve(name: str | None) -> Policy:
    """Look up a policy, falling back to the default.

    An unknown name falls back rather than erroring: screening is on the request path, and
    failing a request because someone typo'd a policy name would trade a security control for
    an outage. The fallback is the *default* policy, never a weaker one.
    """
    if name and name in POLICIES:
        return POLICIES[name]
    return POLICIES[DEFAULT_POLICY]
