"""Toxicity screening. OFF by default.

It is disabled unless GUARDRAILS_TOXICITY_ENABLED=true, and that default is a considered
position rather than an oversight:

  - A keyword-based toxicity filter has a high false-positive rate on exactly the traffic a
    developer platform carries. Security discussions, incident reports and abuse-handling
    workflows are full of words this kind of list flags.
  - Toxicity is contextual in a way PII and credentials are not. "Kill the process" is not
    violence; a quoted customer complaint is not the user being abusive.
  - Shipping it on by default would mean the demo blocks legitimate prompts, and the natural
    reaction -- turning the whole guardrails service off -- loses the PII and secret detection
    that genuinely does work.

So it exists, it is honest about what it is, and an operator opts in. A real deployment would
use a trained classifier (Detoxify, Perspective API, or a provider's own moderation endpoint)
behind this same interface.
"""

from __future__ import annotations

import re

from app.detectors.base import Detector, finding, merge_overlapping
from app.models.schemas import Action, Finding, OWASPCategory, Severity

#: Patterns for content that is unambiguous enough to be worth flagging even with a word list.
#: Slurs are not enumerated here: a list of them in a public repository is its own problem, and
#: a word list is the wrong tool for that job.
PATTERNS: tuple[tuple[str, str, Severity], ...] = (
    ("threat_of_violence",
     r"\b(?:i\s+will|i'm\s+going\s+to|gonna)\s+(?:kill|hurt|attack|stab|shoot)\s+(?:you|him|her|them)\b",
     Severity.HIGH),
    ("self_harm",
     r"\b(?:how\s+to|ways?\s+to|best\s+way\s+to)\s+(?:kill\s+myself|end\s+my\s+life|self[\s-]harm)\b",
     Severity.CRITICAL),
    ("harassment",
     r"\b(?:you\s+are|you're)\s+(?:worthless|pathetic|stupid|an\s+idiot)\b",
     Severity.LOW),
)

COMPILED = tuple((name, re.compile(pattern, re.IGNORECASE), severity) for name, pattern, severity in PATTERNS)


class ToxicityDetector:
    """Keyword-based toxicity screening. Opt-in."""

    name = "toxicity"

    def scan(self, text: str) -> list[Finding]:
        found: list[Finding] = []
        for category, pattern, severity in COMPILED:
            for match in pattern.finditer(text):
                action = Action.BLOCK if severity is Severity.CRITICAL else Action.ALLOW
                found.append(
                    finding(self.name, category, OWASPCategory.LLM02_INSECURE_OUTPUT,
                            severity, action, match, text, confidence=0.5)
                )
        return merge_overlapping(found)


_check: Detector = ToxicityDetector()
