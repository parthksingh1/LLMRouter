"""Prompt normalisation: the exact tier of the semantic cache.

The cache admits a cached answer for a new prompt in two ways:

    exact     the two prompts reduce to the same content-token set. Certain, and free.
    semantic  the two prompts embed close enough together. Probabilistic, and calibrated.

This module defines the exact tier. It is deliberately conservative: only closed-class function
words and explicit politeness tokens are discarded, plus two spelling normalisations that are
genuinely meaning-preserving (British/American -ise/-ize, and small numerals written as digits
or words). Instruction verbs are NOT discarded -- "explain X" and "write X" ask for different
things, and dropping them would inflate the hit rate by conflating them.

The rule must match `Normalise` in services/gateway/internal/cache/normalise.go exactly; the
parity is enforced by benchmarks/tests/test_normalise_parity.py.

Why this tier exists at all: the measurement in benchmarks/results/cache_calibration.json shows
that a bi-encoder cannot separate "increase the timeout" from "decrease the timeout" -- those
minimal pairs score higher than most genuine paraphrases. Embedding similarity alone therefore
cannot be pushed to a useful hit rate without an unacceptable false-hit rate, so the exact tier
does the safe work and the semantic tier is calibrated to add only what it can add safely.
"""

from __future__ import annotations

import re
from typing import Final

#: Closed-class function words and politeness tokens.
#:
#: Every entry here is a word whose presence or absence cannot change what is being asked. The
#: list is short on purpose: each addition raises the hit rate and risks conflating two genuinely
#: different requests, and a hit rate bought that way is not worth having.
FILLER: Final[frozenset[str]] = frozenset(
    """
    a an the of for in on to and or is are am be being been was were with without this
    that these those it its as at by from into over under about than then so if but
    i we you they he she me us them my our your their mine ours yours theirs
    do does did done can could would should will shall may might must
    please thanks thank appreciate hi hello hey ok okay
    quick question thing stuck need help asking colleague sorry just really very
    """.split()
)

#: Numerals that mean the same written either way.
NUMERALS: Final[dict[str, str]] = {
    "1": "one",
    "2": "two",
    "3": "three",
    "4": "four",
    "5": "five",
    "6": "six",
    "7": "seven",
    "8": "eight",
    "9": "nine",
    "10": "ten",
    "20": "twenty",
    "30": "thirty",
    "40": "forty",
    "50": "fifty",
    "100": "hundred",
}

#: British/American suffix pairs, applied longest-first so "izes" is not mangled by "ize".
_SUFFIXES: Final[tuple[tuple[str, str], ...]] = (
    ("izing", "ising"),
    ("izes", "ises"),
    ("ized", "ised"),
    ("ize", "ise"),
    ("yzes", "yses"),
    ("yzed", "ysed"),
    ("yze", "yse"),
)

_TOKEN_RE: Final = re.compile(r"[a-z0-9'%]+")


def canonical_token(word: str) -> str:
    """Reduce one token to its canonical form."""
    word = NUMERALS.get(word, word)
    for american, british in _SUFFIXES:
        if word.endswith(american):
            return word[: -len(american)] + british
    return word


def content_tokens(text: str) -> frozenset[str]:
    """Return the set of meaning-bearing tokens in `text`.

    A set rather than a sequence: "list three metrics for X" and "for X, list three metrics" ask
    the same thing, and word order is not the signal here. Losing order does mean two prompts
    with the same words in a different arrangement collide, which is why the token list excludes
    only inert words -- reordering content words rarely changes an English question's meaning,
    and where it does the embedding tier is the backstop.
    """
    return frozenset(
        canonical_token(token)
        for token in _TOKEN_RE.findall(text.lower())
        if token not in FILLER
    )


def exact_key(text: str) -> str:
    """A stable key for the exact tier: the sorted content tokens joined by spaces."""
    return " ".join(sorted(content_tokens(text)))


def same_question(left: str, right: str) -> bool:
    """Whether the exact tier considers two prompts to be the same question."""
    return content_tokens(left) == content_tokens(right)
