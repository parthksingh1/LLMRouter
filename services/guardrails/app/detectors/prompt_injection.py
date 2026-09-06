"""Prompt-injection screening (OWASP LLM01).

A curated rule set, not a model. That is a deliberate and limited choice, and the limits are
worth stating plainly because this is the detector most likely to be over-trusted:

  - It catches the well-known surface forms: instruction override, role reassignment, system
    prompt extraction, delimiter injection, encoded payloads and the "ignore the above" family.
  - It will not catch a novel or heavily obfuscated attack, and it is not adversarially robust.
    Anyone who knows these rules exist can write around them.

A production deployment replaces the rule set with a trained classifier behind this same
interface -- the scan() signature does not change, only what is behind it. The reason a rule set
ships here is that a classifier is a model artefact: it needs downloading, versioning and a GPU
budget, and it would make `make demo` depend on all three. See docs/adr/0005.

The rules are grouped by what they attempt, because grouping is what makes the output useful:
"instruction_override" tells an operator something, "rule_47" does not.
"""

from __future__ import annotations

import re

from app.detectors.base import Detector, finding, merge_overlapping
from app.models.schemas import Action, Finding, OWASPCategory, Severity


class Rule:
    """One injection pattern."""

    def __init__(self, category: str, pattern: str, severity: Severity, confidence: float) -> None:
        self.category = category
        self.regex = re.compile(pattern, re.IGNORECASE | re.MULTILINE)
        self.severity = severity
        self.confidence = confidence


RULES: tuple[Rule, ...] = (
    # --- instruction override: the classic "ignore your instructions" ------------------
    Rule(
        "instruction_override",
        r"\b(?:ignore|disregard|forget|override)\b[^.\n]{0,40}?\b(?:previous|prior|above|earlier|all)\b"
        r"[^.\n]{0,40}?\b(?:instruction|prompt|rule|direction|command|guideline)s?\b",
        Severity.HIGH,
        0.9,
    ),
    Rule(
        "instruction_override",
        r"\bdisregard\b[^.\n]{0,30}\b(?:everything|anything)\b[^.\n]{0,30}\b(?:said|told|above)\b",
        Severity.HIGH,
        0.85,
    ),
    Rule("instruction_override", r"\bnew\s+(?:instruction|rule|directive)s?\s*:", Severity.MEDIUM, 0.7),
    # --- role reassignment --------------------------------------------------------------
    Rule(
        "role_reassignment",
        r"\byou\s+are\s+now\b[^.\n]{0,60}?\b(?:DAN|unrestricted|jailbroken|developer\s+mode|admin|root)\b",
        Severity.HIGH,
        0.9,
    ),
    Rule(
        "role_reassignment",
        r"\b(?:pretend|act|behave|roleplay)\b[^.\n]{0,30}?\b(?:you\s+have\s+no|without)\b"
        r"[^.\n]{0,30}?\b(?:restriction|limitation|filter|guardrail|rule)s?\b",
        Severity.HIGH,
        0.85,
    ),
    Rule("role_reassignment", r"\benter\s+(?:developer|debug|god|admin)\s+mode\b", Severity.HIGH, 0.85),
    # --- system prompt extraction -------------------------------------------------------
    Rule(
        "system_prompt_extraction",
        r"\b(?:repeat|print|show|reveal|output|display|tell\s+me)\b[^.\n]{0,40}?"
        r"\b(?:system\s+prompts?|initial\s+instructions?|your\s+instructions?|the\s+prompt\s+above)\b",
        Severity.HIGH,
        0.9,
    ),
    Rule(
        "system_prompt_extraction",
        r"\bwhat\s+(?:were|are)\s+(?:you|your)\b[^.\n]{0,30}\b(?:told|instruction|prompt)s?\b",
        Severity.MEDIUM,
        0.7,
    ),
    # --- delimiter and format injection --------------------------------------------------
    Rule("delimiter_injection", r"(?:^|\n)\s*(?:###\s*)?(?:system|assistant)\s*:\s*", Severity.MEDIUM, 0.6),
    Rule("delimiter_injection", r"<\s*/?\s*(?:system|im_start|im_end|\|im_start\|)\s*>", Severity.HIGH, 0.85),
    Rule("delimiter_injection", r"\[\s*(?:INST|/INST|SYSTEM)\s*\]", Severity.MEDIUM, 0.7),
    # --- exfiltration and excessive agency (LLM08) ---------------------------------------
    Rule(
        "exfiltration",
        r"\b(?:send|post|upload|exfiltrate|forward)\b[^.\n]{0,40}?\b(?:to\s+https?://|to\s+the\s+url|webhook)\b",
        Severity.CRITICAL,
        0.8,
    ),
    Rule(
        "exfiltration",
        r"!\[[^\]]*\]\(https?://[^)]*\{\{[^}]*\}\}[^)]*\)",  # markdown image with templating
        Severity.HIGH,
        0.8,
    ),
    # --- encoded payloads -----------------------------------------------------------------
    # A long base64 run inside a prompt is a common way to smuggle instructions past a
    # keyword filter. Flagged, not blocked, because legitimate prompts do contain encoded data.
    Rule(
        "encoded_payload",
        r"\b(?:base64|rot13|hex)\s*(?:decode|decoded|this|the\s+following)\b",
        Severity.MEDIUM,
        0.65,
    ),
    Rule("encoded_payload", r"\b[A-Za-z0-9+/]{120,}={0,2}\b", Severity.LOW, 0.4),
    # --- refusal suppression ---------------------------------------------------------------
    Rule(
        "refusal_suppression",
        r"\b(?:do\s+not|don't|never)\s+(?:refuse|decline|say\s+(?:no|you\s+can't)|apolog)",
        Severity.MEDIUM,
        0.7,
    ),
    Rule(
        "refusal_suppression",
        r"\bwithout\s+(?:any\s+)?(?:warning|disclaimer|caveat|safety)\b",
        Severity.LOW,
        0.5,
    ),
)

#: Categories that map to LLM08 rather than LLM01.
_AGENCY_CATEGORIES = frozenset({"exfiltration"})


class PromptInjectionDetector:
    """Heuristic prompt-injection screening."""

    name = "prompt_injection"

    def scan(self, text: str) -> list[Finding]:
        found: list[Finding] = []

        for rule in RULES:
            owasp = (
                OWASPCategory.LLM08_EXCESSIVE_AGENCY
                if rule.category in _AGENCY_CATEGORIES
                else OWASPCategory.LLM01_PROMPT_INJECTION
            )
            # Injection attempts are blocked rather than redacted: unlike PII, there is no
            # useful residue once the instruction is removed, and rewriting an attack into
            # something that looks benign is worse than refusing it.
            action = Action.BLOCK if rule.severity in (Severity.HIGH, Severity.CRITICAL) else Action.ALLOW

            for match in rule.regex.finditer(text):
                found.append(
                    finding(
                        self.name,
                        rule.category,
                        owasp,
                        rule.severity,
                        action,
                        match,
                        text,
                        confidence=rule.confidence,
                    )
                )

        return merge_overlapping(found)


_check: Detector = PromptInjectionDetector()
