"""PII detection (OWASP LLM06: Sensitive Information Disclosure).

Regex-based, with checksum validation where the format provides one. It is not Presidio: there
is no NER model here, so it will miss a person's name written in free text. That limitation is
stated in the README rather than papered over, because a PII detector that quietly misses things
is worse than one whose blind spots are known -- an operator can compensate for a known gap.

What it does catch reliably are the structured identifiers that actually leak into prompts:
emails, phone numbers, national IDs, payment cards and IBANs. Those are the ones that appear
when a user pastes a support ticket or a customer record into a chat box.
"""

from __future__ import annotations

import re

from app.detectors.base import Detector, finding, merge_overlapping
from app.models.schemas import Action, Finding, OWASPCategory, Severity

# Anchored on word boundaries throughout, so an identifier embedded in a longer token (a UUID,
# a file path) does not produce a false positive.
EMAIL_RE = re.compile(r"\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b")

# International and North American forms. Deliberately requires either a leading +, or a
# separator-bearing local format, so that bare 10-digit numbers (order ids, timestamps) do not
# all register as phone numbers.
PHONE_RE = re.compile(
    r"(?:(?<=\s)|^)(?:\+\d{1,3}[\s.-]?)?(?:\(\d{2,4}\)[\s.-]?|\d{2,4}[\s.-])\d{3,4}[\s.-]?\d{3,4}\b"
)

SSN_RE = re.compile(r"\b(?!000|666|9\d\d)\d{3}-(?!00)\d{2}-(?!0000)\d{4}\b")

# 13-19 digits with optional separators. The Luhn check below is what makes this usable: the
# raw pattern alone matches far too much.
CARD_RE = re.compile(r"\b(?:\d[ -]?){12,18}\d\b")

IBAN_RE = re.compile(r"\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b")

# Only formats with a checkable structure; a bare date is not PII on its own.
IP_RE = re.compile(r"\b(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\b")


def luhn_valid(digits: str) -> bool:
    """Luhn checksum, which every real payment card satisfies.

    This is what turns a pattern that matches any long number into a detector with a usable
    false-positive rate: an order id or a timestamp fails Luhn roughly nine times in ten.
    """
    numbers = [int(c) for c in digits if c.isdigit()]
    if not 13 <= len(numbers) <= 19:
        return False

    total = 0
    parity = len(numbers) % 2
    for i, digit in enumerate(numbers):
        if i % 2 == parity:
            digit *= 2
            if digit > 9:
                digit -= 9
        total += digit
    return total % 10 == 0


def iban_valid(candidate: str) -> bool:
    """ISO 7064 mod-97 check, the IBAN equivalent of Luhn."""
    if len(candidate) < 15:
        return False
    rearranged = candidate[4:] + candidate[:4]
    digits = ""
    for char in rearranged:
        if char.isdigit():
            digits += char
        elif char.isalpha():
            digits += str(ord(char.upper()) - ord("A") + 10)
        else:
            return False
    try:
        return int(digits) % 97 == 1
    except ValueError:  # pragma: no cover - unreachable given the guard above
        return False


class PIIDetector:
    """Finds structured personal identifiers."""

    name = "pii"

    def scan(self, text: str) -> list[Finding]:
        found: list[Finding] = []

        for match in EMAIL_RE.finditer(text):
            found.append(
                finding(
                    self.name,
                    "email",
                    OWASPCategory.LLM06_SENSITIVE_INFO,
                    Severity.MEDIUM,
                    Action.REDACT,
                    match,
                    text,
                )
            )

        for match in SSN_RE.finditer(text):
            found.append(
                finding(
                    self.name,
                    "ssn",
                    OWASPCategory.LLM06_SENSITIVE_INFO,
                    Severity.CRITICAL,
                    Action.REDACT,
                    match,
                    text,
                )
            )

        for match in CARD_RE.finditer(text):
            if not luhn_valid(match.group()):
                continue
            found.append(
                finding(
                    self.name,
                    "credit_card",
                    OWASPCategory.LLM06_SENSITIVE_INFO,
                    Severity.CRITICAL,
                    Action.REDACT,
                    match,
                    text,
                )
            )

        for match in IBAN_RE.finditer(text):
            if not iban_valid(match.group()):
                continue
            found.append(
                finding(
                    self.name,
                    "iban",
                    OWASPCategory.LLM06_SENSITIVE_INFO,
                    Severity.HIGH,
                    Action.REDACT,
                    match,
                    text,
                )
            )

        for match in PHONE_RE.finditer(text):
            digits = sum(c.isdigit() for c in match.group())
            if not 7 <= digits <= 15:
                continue
            found.append(
                finding(
                    self.name,
                    "phone",
                    OWASPCategory.LLM06_SENSITIVE_INFO,
                    Severity.MEDIUM,
                    Action.REDACT,
                    match,
                    text,
                    confidence=0.8,
                )
            )

        for match in IP_RE.finditer(text):
            # Private ranges are infrastructure detail, not personal data, and redacting them
            # would mangle the debugging questions people legitimately ask.
            if match.group().startswith(("10.", "192.168.", "127.", "172.16.", "0.")):
                continue
            found.append(
                finding(
                    self.name,
                    "ip_address",
                    OWASPCategory.LLM06_SENSITIVE_INFO,
                    Severity.LOW,
                    Action.REDACT,
                    match,
                    text,
                    confidence=0.6,
                )
            )

        return merge_overlapping(found)


_check: Detector = PIIDetector()
