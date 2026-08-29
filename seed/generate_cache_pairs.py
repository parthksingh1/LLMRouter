#!/usr/bin/env python3
"""Generate the labelled pair set used to calibrate the semantic cache threshold.

    python seed/generate_cache_pairs.py --seed 1337 --out benchmarks/cache/pairs.jsonl

The file contains 500 positive and 500 negative pairs:

    positive   a paraphrase of the same question. A semantic cache SHOULD hit.
    negative   a minimally different question with high lexical overlap. A cache MUST NOT hit;
               serving the cached answer here would be a false hit -- a wrong answer delivered
               with full confidence, which is the failure mode that makes people distrust
               caching in the first place.

The negatives are deliberately hard. Sampling unrelated questions as negatives would produce a
flattering ROC curve and a threshold that falls apart in production, where the near-misses are
exactly the pairs that differ by one word: "increase" versus "decrease", "read" versus "write",
"before" versus "after".
"""

from __future__ import annotations

import argparse
import json
import random
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from corpus import (  # noqa: E402
    EASY_TEMPLATES,
    HARD_TEMPLATES,
    MEDIUM_TEMPLATES,
    NEGATIVE_MIX,
    Template,
    confuse,
    paraphrase,
    prompt_key,
    render,
)

ALL_TEMPLATES: tuple[Template, ...] = EASY_TEMPLATES + MEDIUM_TEMPLATES + HARD_TEMPLATES


def build_pairs(seed: int, positives: int, negatives: int) -> list[dict[str, Any]]:
    """Build the labelled pair list deterministically."""
    rng = random.Random(seed + 101)  # offset so this stream differs from the eval set's
    pairs: list[dict[str, Any]] = []

    # --- positives ---------------------------------------------------------------------
    seen: set[tuple[str, str]] = set()
    attempts = 0
    while len([p for p in pairs if p["label"] == 1]) < positives:
        attempts += 1
        if attempts > positives * 40:  # pragma: no cover - guards against a degenerate corpus
            raise RuntimeError("could not generate enough distinct positive pairs")

        template = rng.choice(ALL_TEMPLATES)
        left = render(template, rng)
        right = paraphrase(left, rng)
        if right.strip() == left.strip():
            continue

        fingerprint = (prompt_key(left), prompt_key(right))
        if fingerprint in seen:
            continue
        seen.add(fingerprint)

        pairs.append(
            {
                "id": f"pos-{len(pairs):04d}",
                "left": left,
                "right": right,
                "label": 1,
                "kind": "paraphrase",
                "category": template.category,
            }
        )

    # --- hard negatives ----------------------------------------------------------------
    #
    # Composed to the declared NEGATIVE_MIX rather than to whatever the generator happens to
    # produce. Each kind is generated to its own quota, and a kind that cannot fill its quota
    # raises rather than silently borrowing from another -- an unbalanced negative set changes
    # the calibrated threshold, so it must not drift unnoticed.
    quotas = {kind: round(negatives * share) for kind, share in NEGATIVE_MIX.items()}
    # Give any rounding remainder to the commonest kind.
    quotas["different_question"] += negatives - sum(quotas.values())

    for kind, quota in sorted(quotas.items()):
        made = 0
        attempts = 0
        while made < quota:
            attempts += 1
            if attempts > quota * 200:  # pragma: no cover
                raise RuntimeError(
                    f"could not generate {quota} {kind!r} negatives after {attempts} attempts; "
                    f"the template or confusable pool is too small"
                )

            template = rng.choice(ALL_TEMPLATES)
            left = render(template, rng)

            if kind == "minimal_pair":
                right = confuse(left, rng)
                if right is None:
                    continue
            elif kind == "subject_swap":
                right = render(template, rng)
                if right == left:
                    continue
            else:  # different_question: another template, same subject
                other = rng.choice(ALL_TEMPLATES)
                if other is template:
                    continue
                # Re-render the second template with the same seeded subject by reusing the
                # subject already present in `left` where the template exposes one.
                right = render(other, rng)
                if right == left:
                    continue

            fingerprint = (prompt_key(left), prompt_key(right))
            if fingerprint in seen:
                continue
            seen.add(fingerprint)

            pairs.append(
                {
                    "id": f"neg-{kind}-{made:04d}",
                    "left": left,
                    "right": right,
                    "label": 0,
                    "kind": kind,
                    "category": template.category,
                }
            )
            made += 1

    rng.shuffle(pairs)
    return pairs


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--seed", type=int, default=1337)
    parser.add_argument("--positives", type=int, default=500)
    parser.add_argument("--negatives", type=int, default=500)
    parser.add_argument("--out", type=Path, default=Path("benchmarks/cache/pairs.jsonl"))
    args = parser.parse_args()

    pairs = build_pairs(args.seed, args.positives, args.negatives)

    args.out.parent.mkdir(parents=True, exist_ok=True)
    with args.out.open("w", encoding="utf-8", newline="\n") as fh:
        for pair in pairs:
            fh.write(json.dumps(pair, sort_keys=True, ensure_ascii=False) + "\n")

    kinds: dict[str, int] = {}
    for pair in pairs:
        kinds[pair["kind"]] = kinds.get(pair["kind"], 0) + 1

    print(f"wrote {len(pairs)} labelled pairs to {args.out}")
    print(f"  positives : {sum(1 for p in pairs if p['label'] == 1)}")
    print(f"  negatives : {sum(1 for p in pairs if p['label'] == 0)}")
    print(f"  kinds     : {kinds}")
    print(f"  seed      : {args.seed}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
