"""LLMRouter cost and attribution dashboard.

Deliberately small. The point of this page is to answer four questions an engineering manager
actually asks about an LLM bill, and then stop:

  1. Who is spending the money?
  2. Is anything running away, and since when?
  3. Which models is the router picking, and what is that costing?
  4. Is the cache earning its keep?

The anomaly detection is imported from `benchmarks/common/anomaly.py` -- the same module the
attribution benchmark uses -- so the tenants highlighted here and the tenants in
`benchmarks/results/attribution.json` cannot disagree. That is worth more than any amount of
chart polish: a dashboard that quietly disagrees with the report is worse than no dashboard.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path
from typing import Any

import pandas as pd
import streamlit as st

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT))

from benchmarks.common.anomaly import detect  # noqa: E402

CLICKHOUSE_URL = os.getenv("CLICKHOUSE_URL", "http://localhost:8123")
DATABASE = os.getenv("CLICKHOUSE_DB", "llmrouter")
ZSCORE_THRESHOLD = float(os.getenv("ANOMALY_ZSCORE_THRESHOLD", "2.5"))

st.set_page_config(page_title="LLMRouter cost attribution", page_icon="💸", layout="wide")


@st.cache_data(ttl=30)
def query(sql: str) -> pd.DataFrame:
    """Run a query against ClickHouse and return a DataFrame.

    Cached for 30 seconds. The rollup tables make these queries cheap, but a Streamlit page
    re-runs every widget interaction from the top, and without caching a slider drag would fire
    a dozen queries.
    """
    import urllib.parse
    import urllib.request

    params = urllib.parse.urlencode({"database": DATABASE, "default_format": "JSONEachRow"})
    request = urllib.request.Request(
        f"{CLICKHOUSE_URL.rstrip('/')}/?{params}", data=sql.encode("utf-8"), method="POST"
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        body = response.read().decode("utf-8")

    rows = [pd.read_json(line, typ="series") for line in body.splitlines() if line.strip()]
    return pd.DataFrame(rows) if rows else pd.DataFrame()


def money(value: float) -> str:
    return f"${value:,.2f}"


# ---------------------------------------------------------------------------------------------
# Header
# ---------------------------------------------------------------------------------------------

st.title("LLMRouter — cost and attribution")

try:
    totals = query(
        """
        SELECT
            count()                                         AS requests,
            countIf(cached = 1)                             AS cached,
            countIf(guardrail_blocked = 1)                  AS blocked,
            sum(failover_count)                             AS failovers,
            sumIf(cost_usd, cached = 0)                     AS billed_usd,
            sumIf(cost_usd, cached = 1)                     AS saved_usd,
            uniq(tenant_id)                                 AS tenants,
            min(toDate(ts))                                 AS first_day,
            max(toDate(ts))                                 AS last_day
        FROM events
        """
    )
except Exception as exc:  # noqa: BLE001 - the page must explain itself rather than stack-trace
    st.error(
        f"Could not reach ClickHouse at `{CLICKHOUSE_URL}`.\n\n"
        f"Start the stack with `make demo`, which also seeds the data this page reads.\n\n"
        f"Details: {exc}"
    )
    st.stop()

if totals.empty or int(totals.iloc[0]["requests"]) == 0:
    st.warning(
        "ClickHouse is up but has no events yet. Run `bash seed/seed_clickhouse.sh` "
        "(or `make demo`, which does it for you)."
    )
    st.stop()

row = totals.iloc[0]
requests = int(row["requests"])
billed = float(row["billed_usd"])
saved = float(row["saved_usd"])

c1, c2, c3, c4, c5 = st.columns(5)
c1.metric("Requests", f"{requests:,}")
c2.metric("Billed spend", money(billed))
c3.metric(
    "Saved by cache",
    money(saved),
    # The delta is the share of what spend WOULD have been, which is the honest denominator:
    # measuring the saving against post-cache spend would flatter it.
    delta=f"{saved / (billed + saved) * 100:.1f}% of counterfactual" if billed + saved else None,
)
c4.metric("Cache hit rate", f"{int(row['cached']) / requests * 100:.1f}%")
c5.metric("Guardrail blocks", f"{int(row['blocked']):,}")

st.caption(
    f"{row['first_day']} to {row['last_day']} · {int(row['tenants'])} tenants · "
    f"{int(row['failovers']):,} provider failovers · "
    f"cache hits are billed at zero; the saving is the counterfactual cost"
)

# ---------------------------------------------------------------------------------------------
# Anomalies -- the reason this page exists
# ---------------------------------------------------------------------------------------------

st.subheader("Runaway workloads")

daily = query(
    "SELECT toDate(ts) AS day, tenant_id, sumIf(cost_usd, cached = 0) AS cost_usd "
    "FROM events GROUP BY day, tenant_id ORDER BY tenant_id, day"
)

spend_by_tenant: dict[str, dict[str, float]] = {}
for _, r in daily.iterrows():
    spend_by_tenant.setdefault(str(r["tenant_id"]), {})[str(r["day"])] = float(r["cost_usd"])

anomalies = detect(spend_by_tenant, threshold=ZSCORE_THRESHOLD)

if not anomalies:
    st.success("No runaway workloads detected. Every tenant is within its usual range.")
else:
    flagged_spend = sum(a.total_usd for a in anomalies)
    st.error(
        f"**{len(anomalies)} runaway workload(s)** accounting for "
        f"{flagged_spend / billed * 100:.0f}% of billed spend ({money(flagged_spend)})."
    )
    for a in anomalies:
        with st.container(border=True):
            left, right = st.columns([3, 2])
            left.markdown(f"### 🔴 `{a.tenant_id}`")
            left.write(a.explain())
            right.metric("Baseline", f"{money(a.baseline_usd)}/day")
            right.metric(
                "Peak",
                f"{money(a.peak_usd)}/day",
                delta=f"{a.multiple_of_baseline:.0f}x baseline",
                delta_color="inverse",
            )

            series = spend_by_tenant[a.tenant_id]
            chart = pd.DataFrame(
                {"day": list(series.keys()), "cost_usd": list(series.values())}
            ).set_index("day")
            st.line_chart(chart, height=180)

st.caption(
    "Detected by a robust z-score of each day against that tenant's own trailing 14-day median, "
    "requiring both statistical significance and at least a 2x increase. The detector sees only "
    "daily spend — the same signal an on-call engineer has."
)

# ---------------------------------------------------------------------------------------------
# Spend
# ---------------------------------------------------------------------------------------------

st.subheader("Where the money goes")
left, right = st.columns(2)

with left:
    st.markdown("**Top tenants by spend**")
    by_tenant = query(
        "SELECT tenant_id, sumIf(cost_usd, cached = 0) AS billed_usd, count() AS requests, "
        "countIf(cached = 1) / count() AS cache_hit_rate "
        "FROM events GROUP BY tenant_id ORDER BY billed_usd DESC"
    )
    flagged = {a.tenant_id for a in anomalies}
    by_tenant["runaway"] = by_tenant["tenant_id"].isin(flagged)
    by_tenant["share_pct"] = by_tenant["billed_usd"] / billed * 100

    st.dataframe(
        by_tenant,
        hide_index=True,
        use_container_width=True,
        column_config={
            "tenant_id": "Tenant",
            "billed_usd": st.column_config.NumberColumn("Billed", format="$%.2f"),
            "share_pct": st.column_config.ProgressColumn(
                "Share", format="%.1f%%", min_value=0, max_value=float(by_tenant["share_pct"].max())
            ),
            "requests": st.column_config.NumberColumn("Requests", format="%d"),
            "cache_hit_rate": st.column_config.NumberColumn("Cache hits", format="%.0f%%"),
            "runaway": st.column_config.CheckboxColumn("⚠️"),
        },
    )

with right:
    st.markdown("**Model mix**")
    by_model = query(
        "SELECT provider, model, count() AS requests, sumIf(cost_usd, cached = 0) AS billed_usd, "
        "round(quantile(0.5)(latency_ms)) AS p50_ms "
        "FROM events GROUP BY provider, model ORDER BY billed_usd DESC"
    )
    st.dataframe(
        by_model,
        hide_index=True,
        use_container_width=True,
        column_config={
            "provider": "Provider",
            "model": "Model",
            "requests": st.column_config.NumberColumn("Requests", format="%d"),
            "billed_usd": st.column_config.NumberColumn("Billed", format="$%.2f"),
            "p50_ms": st.column_config.NumberColumn("p50", format="%d ms"),
        },
    )
    st.caption(
        "The mix the configured routing policies actually produce — not a target. "
        "A frontier model appearing here on easy traffic is a routing bug."
    )

# ---------------------------------------------------------------------------------------------
# Trends
# ---------------------------------------------------------------------------------------------

st.subheader("Daily spend")
daily_total = query(
    "SELECT toDate(ts) AS day, sumIf(cost_usd, cached = 0) AS billed_usd, "
    "sumIf(cost_usd, cached = 1) AS saved_usd "
    "FROM events GROUP BY day ORDER BY day"
)
if not daily_total.empty:
    st.area_chart(daily_total.set_index("day"), height=240)
    st.caption("`saved_usd` is what the cache hits would have cost. Stacked, the two are the "
               "bill you would have had.")

st.subheader("Cache hit rate over time")
cache = query(
    "SELECT toStartOfHour(ts) AS hour, countIf(cached = 1) / count() AS hit_rate "
    "FROM events GROUP BY hour ORDER BY hour"
)
if not cache.empty:
    st.line_chart(cache.set_index("hour"), height=200)
    st.caption(
        "Hourly rather than daily: a cache regression is a step change that a daily average "
        "smears across 24 hours of good data."
    )

with st.expander("Guardrails"):
    guard = query(
        "SELECT tenant_id, countIf(guardrail_blocked = 1) AS blocked, "
        "sum(guardrail_findings) AS findings, count() AS requests "
        "FROM events GROUP BY tenant_id HAVING findings > 0 ORDER BY blocked DESC"
    )
    if guard.empty:
        st.info("No guardrail findings in this window.")
    else:
        guard["block_rate_pct"] = guard["blocked"] / guard["requests"] * 100
        st.dataframe(guard, hide_index=True, use_container_width=True)

st.divider()
st.caption(
    "Every number here is computed from the `llmrouter.events` table. The anomaly detector is "
    "`benchmarks/common/anomaly.py`, the same module `make bench` runs, so this page and "
    "`benchmarks/results/attribution.json` cannot disagree."
)
