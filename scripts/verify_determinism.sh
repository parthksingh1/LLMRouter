#!/usr/bin/env bash
# Regenerate every committed seed artifact and fail if the bytes changed.
set -euo pipefail
cd "$(dirname "$0")/.."

SEED=${SEED:-1337}
PY=${PY:-python}

targets=(
  "benchmarks/eval/dataset.jsonl"
  "benchmarks/cache/pairs.jsonl"
  "seed/fixtures/responses.jsonl"
)

before=$(mktemp)
for f in "${targets[@]}"; do
  if [ -f "$f" ]; then sha256sum "$f" >> "$before"; fi
done

$PY seed/generate_eval_set.py    --seed "$SEED" --out benchmarks/eval/dataset.jsonl
$PY seed/generate_cache_pairs.py --seed "$SEED" --out benchmarks/cache/pairs.jsonl
$PY seed/generate_fixtures.py    --seed "$SEED" --out seed/fixtures/responses.jsonl

after=$(mktemp)
for f in "${targets[@]}"; do sha256sum "$f" >> "$after"; done

if diff -u "$before" "$after"; then
  echo "OK: all seed artifacts are byte-identical on regeneration (SEED=$SEED)"
else
  echo "FAIL: a seed generator is not deterministic, or a committed artifact is stale." >&2
  echo "      If the generator changed on purpose, commit the regenerated files." >&2
  exit 1
fi
