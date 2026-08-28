#!/usr/bin/env python3
"""Generate the Grafana dashboard JSON.

    python infra/grafana/build_dashboard.py

Written by a script rather than exported from the Grafana UI. A hand-exported dashboard carries
several hundred lines of editor state, absolute panel ids and a datasource uid tied to one
install -- none of which survive code review or a fresh environment. Generating it means the
diff of a dashboard change is the change, and the datasource is a template variable that
resolves wherever it is loaded.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

DS: dict[str, str] = {"type": "prometheus", "uid": "${DS_PROMETHEUS}"}


def panel(
    pid: int,
    title: str,
    x: int,
    y: int,
    w: int,
    h: int,
    targets: list[tuple[str, str]],
    unit: str | None = None,
    ptype: str = "timeseries",
    desc: str = "",
    extra: dict[str, Any] | None = None,
) -> dict[str, Any]:
    p: dict[str, Any] = {
        "id": pid,
        "title": title,
        "type": ptype,
        "description": desc,
        "datasource": DS,
        "gridPos": {"x": x, "y": y, "w": w, "h": h},
        "targets": [
            {"datasource": DS, "expr": expr, "legendFormat": legend, "refId": chr(65 + i)}
            for i, (expr, legend) in enumerate(targets)
        ],
        "fieldConfig": {"defaults": {"custom": {}}, "overrides": []},
    }
    if unit:
        p["fieldConfig"]["defaults"]["unit"] = unit
    if extra:
        p["fieldConfig"]["defaults"].update(extra)
    if ptype == "stat":
        p["options"] = {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "background"}
    return p


def row(pid: int, title: str, y: int) -> dict[str, Any]:
    return {
        "id": pid, "type": "row", "title": title,
        "gridPos": {"x": 0, "y": y, "w": 24, "h": 1}, "collapsed": False,
    }


def build() -> dict[str, Any]:
    panels: list[dict[str, Any]] = []
    y = 0

    # --- RED overview -------------------------------------------------------------------
    panels.append(row(100, "Request health (RED)", y))
    y += 1

    panels += [
        panel(1, "Request rate", 0, y, 6, 6,
              [("sum by (route) (rate(llmrouter_requests_total[5m]))", "{{route}}")],
              unit="reqps",
              desc="Split by route so a burst of /v1/models is not mistaken for load."),
        panel(2, "Error rate", 6, y, 6, 6,
              [("sum(rate(llmrouter_requests_total{status=~\"5xx\"}[5m])) "
                "/ sum(rate(llmrouter_requests_total[5m]))", "5xx"),
               ("sum(rate(llmrouter_requests_total{status=~\"4xx\"}[5m])) "
                "/ sum(rate(llmrouter_requests_total[5m]))", "4xx")],
              unit="percentunit",
              desc="4xx is shown separately: guardrail blocks and budget refusals are 4xx and "
                   "are the system working as designed."),
        panel(3, "Latency by percentile", 12, y, 12, 6,
              [("histogram_quantile(0.50, sum by (le) (rate(llmrouter_request_duration_seconds_bucket[5m])))", "p50"),
               ("histogram_quantile(0.95, sum by (le) (rate(llmrouter_request_duration_seconds_bucket[5m])))", "p95"),
               ("histogram_quantile(0.99, sum by (le) (rate(llmrouter_request_duration_seconds_bucket[5m])))", "p99")],
              unit="s",
              desc="Includes upstream generation time, which dominates everything the gateway does."),
    ]
    y += 6

    panels += [
        panel(4, "Cached vs uncached p50", 0, y, 12, 6,
              [("histogram_quantile(0.50, sum by (le) (rate(llmrouter_request_duration_seconds_bucket{cached=\"true\"}[5m])))", "cached"),
               ("histogram_quantile(0.50, sum by (le) (rate(llmrouter_request_duration_seconds_bucket{cached=\"false\"}[5m])))", "uncached")],
              unit="s", desc="The entire argument for the cache, in one panel."),
        panel(5, "In flight", 12, y, 6, 6,
              [("llmrouter_requests_in_flight", "in flight")],
              desc="A rising floor here with a flat request rate means upstreams are slowing down."),
        panel(6, "Requests by status class", 18, y, 6, 6,
              [("sum by (status) (rate(llmrouter_requests_total[5m]))", "{{status}}")],
              unit="reqps"),
    ]
    y += 6

    # --- routing -------------------------------------------------------------------------
    panels.append(row(200, "Routing and providers", y))
    y += 1

    panels += [
        panel(10, "Model mix", 0, y, 8, 7,
              [("sum by (model) (rate(llmrouter_tokens_total{direction=\"completion\"}[10m]))", "{{model}}")],
              desc="Which models the policy is actually choosing. A frontier model appearing on "
                   "easy traffic is a routing bug, and this is where it shows."),
        panel(11, "Provider health", 8, y, 8, 7,
              [("llmrouter_provider_up", "{{provider}}")],
              desc="1 healthy, 0 unhealthy. From the background health checks, not inferred "
                   "from request outcomes.",
              extra={"max": 1, "min": 0}),
        panel(12, "Circuit breaker state", 16, y, 8, 7,
              [("llmrouter_circuit_breaker_state", "{{provider}}")],
              desc="0 closed, 1 half-open, 2 open. A breaker oscillating between 1 and 2 is a "
                   "provider that is up but broken, which is worse than one that is down.",
              extra={"max": 2, "min": 0}),
    ]
    y += 7

    panels += [
        panel(13, "Failovers", 0, y, 8, 6,
              [("sum by (from, to) (rate(llmrouter_failover_total[10m]))", "{{from}} -> {{to}}")],
              unit="reqps",
              desc="The ones labelled midstream=true recovered a response the client had "
                   "already started reading."),
        panel(14, "Upstream latency by provider", 8, y, 8, 6,
              [("histogram_quantile(0.95, sum by (le, provider) (rate(llmrouter_upstream_duration_seconds_bucket[5m])))", "{{provider}} p95")],
              unit="s"),
        panel(15, "Upstream errors", 16, y, 8, 6,
              [("sum by (provider) (rate(llmrouter_upstream_attempts_total{outcome=\"error\"}[5m]))", "{{provider}}")],
              unit="reqps"),
    ]
    y += 6

    # --- cache, guardrails, budgets -------------------------------------------------------
    panels.append(row(300, "Cache, guardrails and budgets", y))
    y += 1

    panels += [
        panel(20, "Cache hit ratio", 0, y, 6, 6,
              [("llmrouter_cache_hit_ratio", "hit ratio")],
              unit="percentunit", ptype="stat",
              desc="A sustained fall means the embedder or Redis is down, or a deploy changed "
                   "prompt construction so every request looks new."),
        panel(21, "Cache lookups", 6, y, 9, 6,
              [("sum by (result) (rate(llmrouter_cache_lookups_total[5m]))", "{{result}}")],
              unit="reqps",
              desc="`error` is a degraded dependency, not a failed request: a broken cache "
                   "falls through to the provider."),
        panel(22, "Similarity distribution", 15, y, 9, 6,
              [("histogram_quantile(0.50, sum by (le) (rate(llmrouter_cache_similarity_bucket[10m])))", "p50"),
               ("histogram_quantile(0.95, sum by (le) (rate(llmrouter_cache_similarity_bucket[10m])))", "p95")],
              desc="Nearest-neighbour similarity on every lookup, hit or miss. If p95 sits just "
                   "below the threshold, the threshold is set too high."),
    ]
    y += 6

    panels += [
        panel(23, "Guardrail latency", 0, y, 8, 6,
              [("histogram_quantile(0.99, sum by (le) (rate(llmrouter_guardrail_duration_seconds_bucket[5m])))", "p99"),
               ("histogram_quantile(0.50, sum by (le) (rate(llmrouter_guardrail_duration_seconds_bucket[5m])))", "p50")],
              unit="s",
              desc="Budget is a p99 under 12 ms. This sits on every single request."),
        panel(24, "Guardrail blocks by OWASP category", 8, y, 8, 6,
              [("sum by (owasp) (rate(llmrouter_guardrail_block_total[10m]))", "{{owasp}}")],
              unit="reqps"),
        panel(25, "Fail-open events", 16, y, 8, 6,
              [("rate(llmrouter_guardrail_fail_open_total[5m])", "failed open")],
              unit="reqps",
              desc="Requests that reached a provider unscreened because the sidecar was "
                   "unreachable. Non-zero is a security event, even when it is the configured "
                   "trade-off."),
    ]
    y += 6

    panels += [
        panel(30, "Spend rate by tenant", 0, y, 12, 6,
              [("topk(8, sum by (tenant) (rate(llmrouter_cost_usd_total[10m]) * 3600))", "{{tenant}}")],
              unit="currencyUSD",
              desc="Dollars per hour. The live counterpart to the attribution dashboard."),
        panel(31, "Budget outcomes", 12, y, 6, 6,
              [("sum by (action) (rate(llmrouter_budget_block_total[10m]))", "{{action}}")],
              unit="reqps"),
        panel(32, "Analytics events dropped", 18, y, 6, 6,
              [("rate(llmrouter_events_dropped_total[5m])", "dropped")],
              unit="reqps",
              desc="The sink drops rather than blocking a user request. Non-zero means "
                   "ClickHouse is behind and some analytics are being lost."),
    ]

    return {
        "uid": "llmrouter-overview",
        "title": "LLMRouter overview",
        "description": "Generated by infra/grafana/build_dashboard.py. Edit the script, not this file.",
        "tags": ["llmrouter"],
        "timezone": "browser",
        "schemaVersion": 39,
        "version": 1,
        "refresh": "10s",
        "time": {"from": "now-1h", "to": "now"},
        "templating": {
            "list": [
                {
                    "name": "DS_PROMETHEUS",
                    "type": "datasource",
                    "query": "prometheus",
                    "current": {"text": "Prometheus", "value": "Prometheus"},
                    "hide": 0,
                }
            ]
        },
        "panels": panels,
    }


def main() -> int:
    dashboard = build()
    out = Path(__file__).resolve().parent / "dashboards" / "llmrouter.json"
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w", encoding="utf-8", newline="\n") as fh:
        json.dump(dashboard, fh, indent=2)
        fh.write("\n")

    charts = len([p for p in dashboard["panels"] if p["type"] != "row"])
    print(f"wrote {out} with {charts} panels")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
