"""Secret detection (OWASP LLM06).

Two complementary strategies:

  - vendor-prefix patterns, which are high-precision because the prefixes are unambiguous
    (`sk-`, `ghp_`, `AKIA`, `xoxb-`);
  - a Shannon-entropy check for the long random strings that have no recognisable prefix.

The entropy check is the interesting one. Run naively it flags every UUID, hash and base64
blob in every prompt, which makes the detector useless within a day. It is therefore gated on
three things at once: sufficient length, high entropy, AND a mixed character class -- and known
non-secret shapes (UUIDs, hex digests, git SHAs) are excluded explicitly.

Secrets are blocked rather than redacted. A prompt containing a live credential should not be
forwarded at all, even with the credential removed: the surrounding text usually explains what
the credential is for, and the user needs to know it was rejected so they can rotate it.
"""

from __future__ import annotations

import math
import re
from collections import Counter

from app.detectors.base import Detector, finding, merge_overlapping
from app.models.schemas import Action, Finding, OWASPCategory, Severity

#: Vendor patterns. Each is specific enough that a match is almost certainly a real credential.
VENDOR_PATTERNS: tuple[tuple[str, str], ...] = (
    ("openai_key", r"\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}\b"),
    ("anthropic_key", r"\bsk-ant-(?:api\d{2}-)?[A-Za-z0-9_-]{20,}\b"),
    ("aws_access_key", r"\b(?:AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}\b"),
    ("github_token", r"\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{20,}\b"),
    ("gitlab_token", r"\bglpat-[A-Za-z0-9_-]{20,}\b"),
    ("slack_token", r"\bxox[abprs]-[A-Za-z0-9-]{10,}\b"),
    ("stripe_key", r"\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{20,}\b"),
    ("google_api_key", r"\bAIza[A-Za-z0-9_-]{35}\b"),
    ("private_key_block", r"-----BEGIN (?:RSA |EC |OPENSSH |PGP )?PRIVATE KEY-----"),
    ("jwt", r"\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b"),
    ("bearer_header", r"\b[Aa]uthorization\s*:\s*Bearer\s+[A-Za-z0-9._-]{20,}"),
    ("connection_string", r"\b(?:postgres|postgresql|mysql|mongodb(?:\+srv)?|redis|amqp)://[^\s:@]+:[^\s:@]+@[^\s/]+"),
)

COMPILED_VENDOR = tuple((name, re.compile(pattern)) for name, pattern in VENDOR_PATTERNS)

#: Candidate tokens for the entropy check.
HIGH_ENTROPY_CANDIDATE = re.compile(r"\b[A-Za-z0-9+/=_-]{24,}\b")

#: Shapes that look random but are not secrets. Excluding them explicitly is what keeps the
#: entropy check usable: without this it fires on every request id in every debugging prompt.
UUID_RE = re.compile(r"\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b")
HEX_DIGEST_RE = re.compile(r"\b[0-9a-fA-F]{32,64}\b")

#: Entropy floor in bits per character. English prose sits near 2.5-3.5; a random 62-character
#: alphabet tops out near 5.95. 4.2 sits above anything that reads like a word and below any
#: real key.
ENTROPY_FLOOR = 4.2
MIN_SECRET_LENGTH = 24


def shannon_entropy(value: str) -> float:
    """Bits of entropy per character."""
    if not value:
        return 0.0
    counts = Counter(value)
    length = len(value)
    return -sum((c / length) * math.log2(c / length) for c in counts.values())


def looks_like_a_secret(token: str) -> bool:
    """Whether a high-entropy token is plausibly a credential rather than an id or a digest."""
    if len(token) < MIN_SECRET_LENGTH:
        return False
    if UUID_RE.fullmatch(token) or HEX_DIGEST_RE.fullmatch(token):
        return False
    if shannon_entropy(token) < ENTROPY_FLOOR:
        return False

    # A real credential mixes character classes. A long lowercase-only run is far more likely to
    # be an identifier, a slug or a base32 blob than a key.
    has_lower = any(c.islower() for c in token)
    has_upper = any(c.isupper() for c in token)
    has_digit = any(c.isdigit() for c in token)
    return sum((has_lower, has_upper, has_digit)) >= 2


class SecretsDetector:
    """Finds credentials pasted into prompts."""

    name = "secrets"

    def scan(self, text: str) -> list[Finding]:
        found: list[Finding] = []

        for category, pattern in COMPILED_VENDOR:
            for match in pattern.finditer(text):
                found.append(
                    finding(self.name, category, OWASPCategory.LLM06_SENSITIVE_INFO,
                            Severity.CRITICAL, Action.BLOCK, match, text, confidence=0.95)
                )

        for match in HIGH_ENTROPY_CANDIDATE.finditer(text):
            if not looks_like_a_secret(match.group()):
                continue
            found.append(
                finding(self.name, "high_entropy_string", OWASPCategory.LLM06_SENSITIVE_INFO,
                        Severity.HIGH, Action.BLOCK, match, text, confidence=0.6)
            )

        return merge_overlapping(found)


_check: Detector = SecretsDetector()
