"""Shared vocabulary for every seed generator.

Keeping the templates in one module matters for more than tidiness: the eval set, the cache
pair set and the mock fixtures all have to talk about the same prompts, or a fixture lookup
misses and the benchmark silently measures synthesised text instead of seeded text.

Everything here is pure data plus deterministic helpers. No wall clock, no unseeded randomness,
no dependency outside the standard library, so `make seed` reproduces byte-for-byte on any
machine with Python 3.11.
"""

from __future__ import annotations

import hashlib
import random
import re
from dataclasses import dataclass, field
from typing import Final

# --------------------------------------------------------------------------------------
# Token accounting
# --------------------------------------------------------------------------------------

#: Characters per token. This is the same crude estimator the Go gateway uses
#: (internal/providers/mock.EstimateTokens), and they must stay in step: the eval harness
#: prices a call in Python while the gateway prices the same call in Go, and a mismatch would
#: make the two bench modes disagree for no interesting reason.
CHARS_PER_TOKEN: Final = 4


def estimate_tokens(text: str) -> int:
    """Approximate a token count from text, matching the Go implementation exactly."""
    if not text:
        return 0
    return max(1, (len(text) + CHARS_PER_TOKEN - 1) // CHARS_PER_TOKEN)


def normalize_prompt(text: str) -> str:
    """Lowercase and collapse whitespace, matching domain.NormalizePrompt in Go."""
    return " ".join(text.lower().split())


def prompt_key(text: str) -> str:
    """Return the fixture key for a prompt: sha256 of its normalised form, as hex."""
    return hashlib.sha256(normalize_prompt(text).encode("utf-8")).hexdigest()


# --------------------------------------------------------------------------------------
# Difficulty buckets
# --------------------------------------------------------------------------------------

EASY: Final = "easy"
MEDIUM: Final = "medium"
HARD: Final = "hard"

DIFFICULTIES: Final = (EASY, MEDIUM, HARD)

#: The quality a model needs to answer a case correctly, drawn per difficulty.
#:
#: These ranges are the mechanism behind the measured quality drift. A model passes a case iff
#: its measured quality clears the case's threshold, so routing a hard case to a cheap model
#: costs a point of quality, while routing an easy case to a cheap model costs nothing. The
#: ranges overlap on purpose: real difficulty is not cleanly separable, and a classifier that
#: could always tell the buckets apart would make the drift figure meaningless.
#: Each range's upper bound is chosen to sit just below a real model in the catalogue, so that
#: the quality floor for a bucket has an unambiguous cheapest answer:
#:   easy   <= 0.86  -> gpt-4o-mini            (0.863)
#:   medium <= 0.88  -> llama-3.1-70b-instruct (0.884)
#:   hard   <= 0.93  -> gemini-1.5-pro         (0.932)
#: That is what makes the reported quality drift attributable: a correctly classified prompt is
#: always answerable by the model its bucket selects, so every regression is a classifier error
#: rather than an artifact of arbitrary threshold placement.
QUALITY_THRESHOLDS: Final[dict[str, tuple[float, float]]] = {
    EASY: (0.70, 0.860),
    MEDIUM: (0.80, 0.880),
    HARD: (0.88, 0.930),
}


@dataclass(frozen=True)
class Template:
    """One prompt template.

    `body` is formatted with a subject drawn from SUBJECTS. `difficulty` is the ground truth.
    `misleading` marks a template whose surface form points at the wrong bucket -- a terse but
    genuinely hard question, or a long but trivial one. Those are what give the difficulty
    classifier something to get wrong, which is where the measured quality drift comes from.
    """

    body: str
    difficulty: str
    category: str
    misleading: bool = False
    tags: tuple[str, ...] = field(default_factory=tuple)


SUBJECTS: Final[tuple[str, ...]] = (
    "a payment reconciliation service",
    "an event-sourced order pipeline",
    "a multi-tenant search index",
    "a rate limiter shared across regions",
    "a change-data-capture stream",
    "an image thumbnailing worker pool",
    "a feature flag evaluation service",
    "a document ingestion pipeline",
    "a websocket fan-out layer",
    "a nightly ETL job over clickstream data",
    "a distributed job scheduler",
    "a read-through cache in front of Postgres",
    "an email delivery queue",
    "a GraphQL gateway over four microservices",
    "a time-series rollup service",
    "a webhook retry mechanism",
)

LANGUAGES: Final[tuple[str, ...]] = (
    "French",
    "German",
    "Japanese",
    "Portuguese",
    "Hindi",
    "Polish",
)
CITIES: Final[tuple[str, ...]] = (
    "Peru",
    "Norway",
    "Kenya",
    "Vietnam",
    "Chile",
    "Hungary",
    "Morocco",
)
UNITS: Final[tuple[str, ...]] = (
    "miles to kilometres",
    "Celsius to Fahrenheit",
    "pounds to kilograms",
)

# --------------------------------------------------------------------------------------
# Templates
#
# Easy templates carry the markers the classifier treats as easy ("what is", "translate",
# "summarise"). Hard templates carry hard markers ("derive", "trade-off", "race condition").
# Medium templates carry neither, which is exactly why they land in the middle.
# --------------------------------------------------------------------------------------

EASY_TEMPLATES: Final[tuple[Template, ...]] = (
    Template("What is the capital of {city}?", EASY, "factual"),
    Template(
        "Translate this sentence into {language}: the deployment finished successfully.",
        EASY,
        "translation",
    ),
    Template(
        "Summarise the following release note in one sentence: we fixed a bug in {subject}.",
        EASY,
        "summarisation",
    ),
    Template(
        "Define the term idempotency as it applies to {subject}.", EASY, "definition"
    ),
    Template("List three metrics worth tracking for {subject}.", EASY, "listing"),
    Template("Convert {unit} and show the formula.", EASY, "conversion"),
    Template("What is the difference between a queue and a topic?", EASY, "factual"),
    Template(
        "Rephrase this status update to be more concise: {subject} is currently degraded.",
        EASY,
        "rewriting",
    ),
    Template(
        "Spell out the acronym SLA and say what it means for {subject}.",
        EASY,
        "definition",
    ),
    Template("What is a reasonable default timeout for {subject}?", EASY, "factual"),
)

MEDIUM_TEMPLATES: Final[tuple[Template, ...]] = (
    Template(
        "Write a runbook entry for the on-call engineer covering the three most common failures in {subject}.",
        MEDIUM,
        "operations",
    ),
    Template(
        "Draft a short design note explaining how {subject} should handle backpressure.",
        MEDIUM,
        "design",
    ),
    Template(
        "Given a spike in p99 latency on {subject}, outline the first four things you would check.",
        MEDIUM,
        "operations",
    ),
    Template(
        "Write unit test cases covering the boundary conditions of {subject}.",
        MEDIUM,
        "testing",
    ),
    Template(
        "Explain to a new team member how {subject} recovers after a deploy is rolled back.",
        MEDIUM,
        "explanation",
    ),
    Template(
        "Produce a migration checklist for moving {subject} from one region to two.",
        MEDIUM,
        "planning",
    ),
    Template(
        "Write the alerting rules you would configure for {subject}, with thresholds and reasoning.",
        MEDIUM,
        "operations",
    ),
    Template(
        "Describe how you would add idempotency keys to {subject} without downtime.",
        MEDIUM,
        "design",
    ),
    Template(
        "Review this plan for {subject} and list the assumptions it leaves unstated.",
        MEDIUM,
        "review",
    ),
    Template(
        "Write a postmortem summary for an incident where {subject} dropped 2% of messages.",
        MEDIUM,
        "writing",
    ),
)

HARD_TEMPLATES: Final[tuple[Template, ...]] = (
    Template(
        "Derive the worst-case complexity of the retry strategy in {subject}, then analyse the trade-off against a bounded queue.",
        HARD,
        "analysis",
    ),
    Template(
        "Debug the race condition that appears in {subject} under concurrent writes, and explain the root cause.",
        HARD,
        "debugging",
    ),
    Template(
        "Prove that the deduplication scheme in {subject} is correct under at-least-once delivery, step by step.",
        HARD,
        "proof",
    ),
    Template(
        "Architect a rewrite of {subject} for a distributed deployment, and justify each trade-off you make.",
        HARD,
        "architecture",
    ),
    Template(
        "Explain step by step how to optimise the hot path of {subject} without changing its public contract.",
        HARD,
        "optimisation",
    ),
    Template(
        "Analyse the concurrency model of {subject} and identify where a race condition could be introduced by a naive refactor.",
        HARD,
        "analysis",
    ),
    Template(
        "Derive the algorithm needed to shard {subject} while preserving ordering guarantees.",
        HARD,
        "algorithm",
    ),
    Template(
        "Compare two designs for {subject} on complexity, failure modes and cost, and recommend one with reasoning.",
        HARD,
        "architecture",
    ),
)

#: Templates whose surface form misleads the difficulty classifier.
#:
#: The first group is genuinely hard but phrased without any hard marker, so the classifier is
#: likely to under-route them. The second group is trivial but long and code-adjacent, so the
#: classifier is likely to over-route them. Real traffic contains both, and a benchmark that
#: excluded them would report a quality drift far better than production would see.
MISLEADING_TEMPLATES: Final[tuple[Template, ...]] = (
    # Terse but genuinely hard: no hard marker at all, so the classifier reads them as medium.
    Template("Why is {subject} slow?", HARD, "analysis", misleading=True),
    Template("Fix the ordering bug in {subject}.", HARD, "debugging", misleading=True),
    Template("Is this design for {subject} correct?", HARD, "review", misleading=True),
    # Hard questions wearing easy clothes. These carry an EASY marker ("what is", "list",
    # "summarise") while asking for real analysis, so a keyword classifier actively routes them
    # down rather than merely failing to route them up. This is the characteristic failure of
    # any lexical classifier and it is where most of the measured quality drift comes from --
    # excluding it would make the drift figure look far better than production would.
    Template("What is wrong with {subject}?", HARD, "analysis", misleading=True),
    Template(
        "List the reasons {subject} loses messages under load.",
        HARD,
        "analysis",
        misleading=True,
    ),
    Template(
        "Summarise why {subject} deadlocks when two workers retry at once.",
        HARD,
        "debugging",
        misleading=True,
    ),
    Template(
        "What is the right way to shard {subject} without breaking ordering?",
        HARD,
        "architecture",
        misleading=True,
    ),
    Template(
        "Define the correctness condition {subject} must hold under partial failure.",
        HARD,
        "proof",
        misleading=True,
    ),
    Template(
        "Here is a configuration file for {subject}:\n\n"
        "    retries: 3\n"
        "    timeout_ms: 250\n"
        "    backoff: exponential\n\n"
        "What is the value of the timeout?",
        EASY,
        "extraction",
        misleading=True,
    ),
    Template(
        "The following log lines come from {subject}:\n\n"
        "    INFO  worker started\n"
        "    INFO  processed 1024 records\n"
        "    WARN  queue depth 4096\n\n"
        "List the log levels that appear.",
        EASY,
        "extraction",
        misleading=True,
    ),
)


def render(template: Template, rng: random.Random) -> str:
    """Fill a template's placeholders from the seeded RNG."""
    return template.body.format(
        subject=rng.choice(SUBJECTS),
        language=rng.choice(LANGUAGES),
        city=rng.choice(CITIES),
        unit=rng.choice(UNITS),
    )


# --------------------------------------------------------------------------------------
# Paraphrasing, for the cache pair set
# --------------------------------------------------------------------------------------

#: Surface rewrites that preserve meaning. A semantic cache is supposed to hit on these.
PARAPHRASE_PREFIXES: Final[tuple[str, ...]] = (
    "",
    "Quick question: ",
    "Hi, ",
    "Could you help me with this - ",
    "I need to know: ",
    "One thing I'm stuck on: ",
)

PARAPHRASE_SUFFIXES: Final[tuple[str, ...]] = (
    "",
    " Thanks!",
    " Please be brief.",
    " (asking for a colleague)",
    " Appreciate it.",
)

#: Word-level synonym swaps that keep the meaning intact.
SYNONYMS: Final[tuple[tuple[str, str], ...]] = (
    ("What is", "What's"),
    ("Summarise", "Summarize"),
    ("Explain", "Walk me through"),
    ("List", "Give me a list of"),
    ("Describe", "Talk me through"),
    ("outline", "sketch out"),
    ("Write", "Draft"),
    ("analyse", "analyze"),
    ("three", "3"),
    ("Define", "What does it mean by"),
)

#: Substitutions that change what is being asked while keeping the sentence almost identical.
#:
#: These are the minimal pairs: the hardest thing a semantic cache has to get right, because
#: the two prompts are one word apart and an embedding model puts them very close together.
#: Serving one answer for the other is a false hit -- a confidently wrong answer -- which is the
#: failure the threshold calibration exists to bound.
#:
#: The list covers terms that actually appear in the templates above; a confusable nobody uses
#: contributes nothing to the pair set.
CONFUSABLE: Final[tuple[tuple[str, str], ...]] = (
    ("capital", "currency"),
    ("timeout", "retry count"),
    ("queue", "topic"),
    ("latency", "throughput"),
    ("three", "thirty"),
    ("four", "forty"),
    ("increase", "decrease"),
    ("before", "after"),
    ("read", "write"),
    ("enable", "disable"),
    ("maximum", "minimum"),
    ("French", "German"),
    ("Peru", "Norway"),
    ("idempotency", "atomicity"),
    ("backpressure", "batching"),
    ("rolled back", "rolled forward"),
    ("p99", "p50"),
    ("boundary conditions", "happy path"),
    ("unit test", "integration test"),
    ("on-call engineer", "product manager"),
    ("migration checklist", "rollback checklist"),
    ("alerting rules", "dashboard panels"),
    ("postmortem", "design review"),
    ("runbook", "architecture diagram"),
    ("most common", "least common"),
    ("without downtime", "during a maintenance window"),
    ("one region to two", "two regions to one"),
    ("dropped", "duplicated"),
    ("concise", "more detailed"),
    ("in one sentence", "in five paragraphs"),
    ("degraded", "fully recovered"),
    ("default", "maximum permitted"),
    ("difference between", "similarity between"),
    ("metrics worth tracking", "metrics safe to ignore"),
    ("recovers after", "fails during"),
    ("assumptions it leaves unstated", "assumptions it states explicitly"),
)

#: How the hard negatives are composed.
#:
#: The mix is stated explicitly because it is the single biggest influence on the calibrated
#: threshold, and because an earlier version got it wrong by accident: word swaps were attempted
#: first and silently skipped whenever a template contained no confusable term, leaving 73% of
#: the negatives as subject swaps. That made the set far more adversarial than real traffic and
#: pinned the achievable hit rate near zero. Getting this wrong in the flattering direction
#: would have been just as easy, which is why it is now a declared constant rather than an
#: emergent property of the generator.
NEGATIVE_MIX: Final[dict[str, float]] = {
    # One word changed, meaning changed. The hardest case and the most valuable to price.
    "minimal_pair": 0.40,
    # The same question asked about a different system.
    "subject_swap": 0.25,
    # A different question about the same system: the commonest near-miss in real traffic,
    # where a user asks a follow-up about the thing they were already asking about.
    "different_question": 0.35,
}


def paraphrase(text: str, rng: random.Random) -> str:
    """Return a meaning-preserving rewrite of `text`."""
    out = text
    for old, new in SYNONYMS:
        if old in out and rng.random() < 0.55:
            out = out.replace(old, new, 1)
            break
    prefix = rng.choice(PARAPHRASE_PREFIXES)
    suffix = rng.choice(PARAPHRASE_SUFFIXES)
    if prefix and out:
        out = prefix + out[0].lower() + out[1:]
    return out + suffix


def confuse(text: str, rng: random.Random) -> str | None:
    """Return a minimally different but semantically distinct version of `text`.

    Returns None when no confusable term is present, so the caller can pick another source
    rather than emitting a "hard negative" that is really just a paraphrase.
    """
    candidates = [
        (old, new)
        for old, new in CONFUSABLE
        if re.search(rf"\b{re.escape(old)}\b", text)
    ]
    if not candidates:
        return None
    old, new = rng.choice(candidates)
    return re.sub(rf"\b{re.escape(old)}\b", new, text, count=1)
