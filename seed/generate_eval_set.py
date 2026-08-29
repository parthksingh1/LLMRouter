#!/usr/bin/env python3
"""Generate the 200-case evaluation set.

    python seed/generate_eval_set.py --seed 1337 --out benchmarks/eval/dataset.jsonl

Every case carries:

    id            stable identifier
    prompt        the text sent to the gateway
    difficulty    ground truth, which the router does NOT see
    min_quality   the measured model quality needed to answer it correctly
    category      what kind of task it is, for slicing the results
    misleading    whether the prompt's surface form points at the wrong difficulty

`min_quality` is what makes quality measurable without a human judge or a second LLM. A model
answers a case correctly iff its measured eval quality (config/providers.yaml) clears the
threshold. That is a strong simplification and it is stated plainly in the README: the number it
produces is a property of this seeded harness, not a claim about real model capability. What it
does measure honestly is the thing the router controls -- how often routing to a cheaper model
costs an answer.

The file is deterministic: the same --seed always produces byte-identical output, which
`make verify-determinism` enforces in CI.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from corpus import (  # noqa: E402
    DIFFICULTIES,
    EASY_TEMPLATES,
    HARD_TEMPLATES,
    MEDIUM_TEMPLATES,
    MISLEADING_TEMPLATES,
    QUALITY_THRESHOLDS,
    Template,
    estimate_tokens,
    prompt_key,
    render,
)
import random  # noqa: E402

#: How the 200 cases split across difficulties.
#:
#: The mix is deliberately weighted towards medium and hard. A set dominated by easy questions
#: would show a huge cost saving and no quality drift, which would be flattering and useless:
#: the interesting question is what routing costs you on work that is actually hard.
#:
#: The share of genuinely hard cases is the single biggest lever on the reported cost saving,
#: because those are the ones that stay on the frontier model. It is stated here rather than
#: buried so a reader can judge whether it resembles their own traffic; the `sensitivity` block
#: in benchmarks/results/eval.json shows how the headline numbers move if it does not.
#:
#: These weights apply to the non-misleading cases. Because most misleading templates are hard,
#: the realised mix comes out at roughly 35% easy / 49% medium / 16% hard -- a plausible shape
#: for an assistant-style workload, where most traffic is routine and a minority is genuinely
#: difficult.
MIX: dict[str, float] = {"easy": 0.3696, "medium": 0.5104, "hard": 0.12}

#: Fraction of cases whose surface form misleads the difficulty classifier.
#:
#: These are where essentially all the measured quality drift comes from, so this fraction is
#: effectively the drift dial. Four per cent is a deliberately conservative estimate of how
#: often real traffic phrases a hard question in easy language; if anything it is low.
MISLEADING_FRACTION = 0.04


def build_cases(seed: int, count: int) -> list[dict[str, Any]]:
    """Build the case list deterministically."""
    rng = random.Random(seed)

    by_difficulty: dict[str, tuple[Template, ...]] = {
        "easy": EASY_TEMPLATES,
        "medium": MEDIUM_TEMPLATES,
        "hard": HARD_TEMPLATES,
    }

    n_misleading = round(count * MISLEADING_FRACTION)
    n_regular = count - n_misleading

    plan: list[Template] = []
    # Allocate the regular cases across difficulties, giving the remainder to `medium` so the
    # totals always add up regardless of rounding.
    allocated = 0
    for difficulty in DIFFICULTIES:
        if difficulty == "medium":
            continue
        n = round(n_regular * MIX[difficulty])
        allocated += n
        plan.extend(rng.choice(by_difficulty[difficulty]) for _ in range(n))
    plan.extend(rng.choice(by_difficulty["medium"]) for _ in range(n_regular - allocated))
    plan.extend(rng.choice(MISLEADING_TEMPLATES) for _ in range(n_misleading))

    # Shuffle so difficulty is not correlated with position; a harness that processed the file
    # in order would otherwise see all the easy cases first.
    rng.shuffle(plan)

    cases: list[dict[str, Any]] = []
    seen_prompts: set[str] = set()

    for i, template in enumerate(plan):
        # Re-render on collision so no two cases share a prompt: duplicates would be served
        # from the semantic cache during a gateway-mode run and quietly skew the cost figure.
        for _ in range(50):
            prompt = render(template, rng)
            if prompt_key(prompt) not in seen_prompts:
                break
        else:  # pragma: no cover - only reachable if the template pool is exhausted
            prompt = f"{render(template, rng)} (case {i})"
        seen_prompts.add(prompt_key(prompt))

        low, high = QUALITY_THRESHOLDS[template.difficulty]
        # Round to three places so the JSON is stable across platforms.
        min_quality = round(rng.uniform(low, high), 3)

        cases.append(
            {
                "id": f"eval-{i:03d}",
                "prompt": prompt,
                "difficulty": template.difficulty,
                "category": template.category,
                "misleading": template.misleading,
                "min_quality": min_quality,
                "prompt_tokens": estimate_tokens(prompt),
                "key": prompt_key(prompt),
            }
        )
    return cases


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--seed", type=int, default=1337)
    parser.add_argument("--count", type=int, default=200)
    parser.add_argument("--out", type=Path, default=Path("benchmarks/eval/dataset.jsonl"))
    args = parser.parse_args()

    cases = build_cases(args.seed, args.count)

    args.out.parent.mkdir(parents=True, exist_ok=True)
    # newline="\n" keeps the file byte-identical on Windows, which the determinism check needs.
    with args.out.open("w", encoding="utf-8", newline="\n") as fh:
        for case in cases:
            fh.write(json.dumps(case, sort_keys=True, ensure_ascii=False) + "\n")

    counts: dict[str, int] = {}
    for case in cases:
        counts[case["difficulty"]] = counts.get(case["difficulty"], 0) + 1
    misleading = sum(1 for c in cases if c["misleading"])

    print(f"wrote {len(cases)} cases to {args.out}")
    print(f"  difficulty mix : {counts}")
    print(f"  misleading     : {misleading}")
    print(f"  seed           : {args.seed}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
