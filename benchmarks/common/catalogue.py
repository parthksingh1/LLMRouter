"""Reading the provider, policy and tenant catalogues.

The benchmarks load exactly the same YAML files the Go gateway loads. That is the property that
makes the offline benchmark mode trustworthy: when someone changes a price or a quality floor in
`config/`, the reported cost saving moves with it, and nobody has to remember to update a second
copy of the numbers.
"""

from __future__ import annotations

from dataclasses import dataclass
from functools import lru_cache
from pathlib import Path
from typing import Any

import yaml

REPO_ROOT = Path(__file__).resolve().parents[2]
CONFIG_DIR = REPO_ROOT / "config"

#: The token shape used to compare model prices before a call has run. Mirrors
#: estimatePromptTokens/estimateCompletionTokens in services/gateway/internal/router/engine.go;
#: the two must stay in step or the offline and gateway benchmark modes will disagree about
#: which model is cheapest.
ESTIMATE_PROMPT_TOKENS = 350
ESTIMATE_COMPLETION_TOKENS = 280


@dataclass(frozen=True)
class Model:
    """One concrete model from config/providers.yaml."""

    id: str
    provider: str
    family: str
    tier: str
    quality: float
    context: int
    price_in_per_m: float
    price_out_per_m: float

    def cost_usd(self, prompt_tokens: int, completion_tokens: int) -> float:
        """Price a call. Mirrors domain.ModelDescriptor.CostUSD in Go."""
        return (
            prompt_tokens / 1_000_000 * self.price_in_per_m
            + completion_tokens / 1_000_000 * self.price_out_per_m
        )

    @property
    def estimated_cost(self) -> float:
        """The cost the router uses to rank models before a call has run."""
        return self.cost_usd(ESTIMATE_PROMPT_TOKENS, ESTIMATE_COMPLETION_TOKENS)


@dataclass(frozen=True)
class Provider:
    """One provider from config/providers.yaml."""

    name: str
    prefix_continuation: str
    ttfb_p50_ms: int
    ttfb_p95_ms: int
    ttfb_p99_ms: int
    tokens_per_sec: float
    models: tuple[Model, ...]


@dataclass(frozen=True)
class Catalogue:
    """The parsed provider catalogue."""

    providers: tuple[Provider, ...]
    stream_stall_timeout_ms: int
    request_timeout_ms: int

    @property
    def models(self) -> tuple[Model, ...]:
        """Every chat-capable model, excluding embedding models."""
        return tuple(
            m for p in self.providers for m in p.models if m.tier != "embedding"
        )

    def model(self, model_id: str) -> Model:
        for m in self.models:
            if m.id == model_id:
                return m
        raise KeyError(f"model {model_id!r} is not in config/providers.yaml")

    def provider(self, name: str) -> Provider:
        for p in self.providers:
            if p.name == name:
                return p
        raise KeyError(f"provider {name!r} is not in config/providers.yaml")


def _load_yaml(path: Path) -> dict[str, Any]:
    with path.open(encoding="utf-8") as fh:
        data = yaml.safe_load(fh)
    if not isinstance(data, dict):
        raise ValueError(f"{path} did not parse to a mapping")
    return data


@lru_cache(maxsize=1)
def load_catalogue(path: Path | None = None) -> Catalogue:
    """Load config/providers.yaml."""
    raw = _load_yaml(path or CONFIG_DIR / "providers.yaml")
    defaults = raw.get("defaults", {})

    providers: list[Provider] = []
    for p in raw["providers"]:
        latency = p.get("latency", {})
        models = tuple(
            Model(
                id=m["id"],
                provider=p["name"],
                family=m.get("family", ""),
                tier=m.get("tier", ""),
                quality=float(m.get("quality", 0.0)),
                context=int(m.get("context", 0)),
                price_in_per_m=float(m.get("price_in_per_m", 0.0)),
                price_out_per_m=float(m.get("price_out_per_m", 0.0)),
            )
            for m in p["models"]
        )
        providers.append(
            Provider(
                name=p["name"],
                prefix_continuation=p.get("prefix_continuation", "none"),
                ttfb_p50_ms=int(latency.get("ttfb_p50_ms", 300)),
                ttfb_p95_ms=int(latency.get("ttfb_p95_ms", 600)),
                ttfb_p99_ms=int(latency.get("ttfb_p99_ms", 1200)),
                tokens_per_sec=float(latency.get("tokens_per_sec", 60)),
                models=models,
            )
        )

    return Catalogue(
        providers=tuple(providers),
        stream_stall_timeout_ms=int(defaults.get("stream_stall_timeout_ms", 4000)),
        request_timeout_ms=int(defaults.get("request_timeout_ms", 60000)),
    )


@lru_cache(maxsize=1)
def load_policies(path: Path | None = None) -> dict[str, Any]:
    """Load config/policies.yaml."""
    return _load_yaml(path or CONFIG_DIR / "policies.yaml")


@lru_cache(maxsize=1)
def load_tenants(path: Path | None = None) -> dict[str, Any]:
    """Load config/tenants.yaml, applying the defaults block the way the gateway does."""
    raw = _load_yaml(path or CONFIG_DIR / "tenants.yaml")
    defaults = raw.get("defaults", {})
    for tenant in raw.get("tenants", []):
        for key, value in defaults.items():
            tenant.setdefault(key, value)
    return raw


def policy(name: str) -> dict[str, Any]:
    """Return one policy definition by name."""
    for p in load_policies()["policies"]:
        if p["name"] == name:
            return p
    raise KeyError(f"policy {name!r} is not in config/policies.yaml")


def baseline_model() -> Model:
    """The model the cost saving is measured against.

    The baseline is "send everything to the best frontier model", which is what an application
    does before it has a gateway. It is chosen as the highest-quality model in the catalogue
    rather than hard-coded, so replacing gpt-4o with something better moves the baseline too.
    """
    return max(load_catalogue().models, key=lambda m: (m.quality, -m.estimated_cost))
