"""Detector protocol and shared helpers.

A detector is a pure function from text to findings. It holds no state between calls, does no
I/O, and never raises for ordinary input -- the screening path is on every request, so a
detector that can block or throw is a detector that can take the gateway down.
"""

from __future__ import annotations

import hashlib
import re
from typing import Protocol, runtime_checkable

from app.models.schemas import Action, Finding, OWASPCategory, Severity


@runtime_checkable
class Detector(Protocol):
    """One screening rule set."""

    name: str

    def scan(self, text: str) -> list[Finding]:
        """Return every finding in `text`. Must not raise for any string input."""
        ...


def excerpt_hash(matched: str) -> str:
    """Fingerprint a matched span without transmitting it.

    Findings travel to the gateway, into its logs and into the analytics event. Putting the
    matched text in there would copy the secret or the PII to three more places, which is
    precisely what this service exists to prevent -- so callers get a hash they can correlate on
    instead.
    """
    return hashlib.sha256(matched.encode("utf-8")).hexdigest()[:32]


def finding(
    detector: str,
    category: str,
    owasp: OWASPCategory,
    severity: Severity,
    action: Action,
    match: re.Match[str] | tuple[int, int],
    text: str,
    confidence: float = 1.0,
) -> Finding:
    """Build a Finding from a regex match or an explicit span."""
    if isinstance(match, tuple):
        start, end = match
    else:
        start, end = match.start(), match.end()

    return Finding(
        detector=detector,
        category=category,
        owasp=owasp,
        severity=severity,
        action=action,
        start=start,
        end=end,
        excerpt_hash=excerpt_hash(text[start:end]),
        confidence=confidence,
    )


def merge_overlapping(findings: list[Finding]) -> list[Finding]:
    """Collapse findings whose spans overlap, keeping the most severe.

    Without this, "my card is 4111 1111 1111 1111" reports both a credit card and a long-digit
    match, and redaction would then rewrite the same span twice and corrupt the offsets.
    """
    if not findings:
        return []

    order = {Severity.LOW: 0, Severity.MEDIUM: 1, Severity.HIGH: 2, Severity.CRITICAL: 3}
    ordered = sorted(findings, key=lambda f: (f.start, -f.end))

    kept: list[Finding] = []
    for candidate in ordered:
        if kept and candidate.start < kept[-1].end:
            previous = kept[-1]
            # Overlap: keep whichever is more severe, and widen to cover both spans so the
            # redaction still removes everything sensitive.
            winner = candidate if order[candidate.severity] > order[previous.severity] else previous
            kept[-1] = winner.model_copy(
                update={"start": min(previous.start, candidate.start), "end": max(previous.end, candidate.end)}
            )
        else:
            kept.append(candidate)
    return kept


def redact(text: str, findings: list[Finding]) -> str:
    """Replace every redactable finding with a category placeholder.

    Applied right to left so that earlier offsets stay valid while rewriting -- doing it left to
    right silently corrupts every span after the first replacement of a different length.
    """
    redactable = [f for f in findings if f.action is Action.REDACT]
    if not redactable:
        return text

    out = text
    for f in sorted(redactable, key=lambda f: f.start, reverse=True):
        out = f"{out[: f.start]}[REDACTED:{f.category.upper()}]{out[f.end :]}"
    return out
