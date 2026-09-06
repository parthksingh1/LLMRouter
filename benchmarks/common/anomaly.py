"""Finding runaway workloads in a spend series.

Used by both `benchmarks/attribution/run_attribution.py` and the Streamlit dashboard, so the
number the benchmark reports and the number a human sees on the dashboard cannot disagree.

The detector is a robust z-score of each day against that tenant's own TRAILING history. Four
properties matter and each is a deliberate choice:

1. **Per tenant, not across tenants.** A tenant that always spends 50x more than the others is
   not an anomaly, it is a big customer. What matters is a tenant departing from *its own*
   baseline.

2. **A trailing window, not the whole series.** This is the one that took a rewrite to get
   right. A real runaway is not a one-day spike -- it starts when a bad deploy ships and then
   persists. Scored against the whole month, those elevated days end up *being* the baseline,
   and the first version of this detector found none of the three planted runaways for exactly
   that reason. Comparing each day against the days before it detects the regime change instead.

3. **Median and MAD, not mean and standard deviation.** Even inside a trailing window a mean is
   dragged towards an outlier at the window's edge. The median barely moves.

4. **A spend floor.** A tenant that spent $0.002 and then $0.30 has a huge z-score and is not
   worth anyone's attention. Without a floor the alert list fills with rounding noise.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Sequence

#: Scaling factor that makes the MAD a consistent estimator of the standard deviation for
#: normally distributed data, so the threshold is interpretable on the usual z-score scale.
MAD_TO_SIGMA = 1.4826

#: Default z-score above which a day is flagged.
DEFAULT_THRESHOLD = 2.5

#: A flagged day must also be at least this multiple of the trailing baseline.
#:
#: Statistical significance is not the same as mattering. A tenant whose spend is metronomically
#: steady has a tiny MAD, so a 20% wobble scores three sigma and gets flagged -- and an alert
#: list full of 1.2x "anomalies" is one nobody reads. This is the criterion that turns the
#: detector from a significance test into a usable alert.
DEFAULT_MIN_MULTIPLE = 2.0

#: Days below this daily spend are never flagged, however extreme the ratio.
#:
#: Deliberately low, because the demo's seeded month is small. In a real deployment this is the
#: number to raise first: it is what stops the alert list filling with tenants whose spend went
#: from nothing to slightly more than nothing.
DEFAULT_MIN_DAILY_USD = 0.05

#: How many prior days form the baseline a day is compared against.
#:
#: Two weeks is long enough to average out the weekly cycle and short enough that a regime
#: change from three weeks ago has left the window.
DEFAULT_WINDOW_DAYS = 14

#: A tenant needs at least this many days of history before its baseline means anything.
MIN_HISTORY_DAYS = 10

#: Days of history required before scoring starts, so the first few days of a new tenant are not
#: all flagged against an empty baseline.
MIN_WARMUP_DAYS = 5


@dataclass
class Anomaly:
    """One tenant flagged as anomalous."""

    tenant_id: str
    baseline_usd: float
    peak_usd: float
    total_usd: float
    share_of_spend_pct: float
    max_zscore: float
    anomalous_days: list[str] = field(default_factory=list)
    onset_day: str | None = None

    @property
    def multiple_of_baseline(self) -> float:
        return (
            self.peak_usd / self.baseline_usd if self.baseline_usd > 0 else float("inf")
        )

    def explain(self) -> str:
        """A one-line explanation an on-call engineer can act on."""
        onset = f" from {self.onset_day}" if self.onset_day else ""
        return (
            f"{self.tenant_id}: spend rose to ${self.peak_usd:,.2f}/day{onset}, "
            f"{self.multiple_of_baseline:.1f}x its ${self.baseline_usd:,.2f} baseline "
            f"(peak z={self.max_zscore:.1f}); {self.share_of_spend_pct:.1f}% of total spend"
        )


def median(values: Sequence[float]) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    mid = len(ordered) // 2
    if len(ordered) % 2:
        return ordered[mid]
    return (ordered[mid - 1] + ordered[mid]) / 2


def mad(values: Sequence[float], centre: float | None = None) -> float:
    """Median absolute deviation."""
    if not values:
        return 0.0
    c = median(values) if centre is None else centre
    return median([abs(v - c) for v in values])


def robust_zscores(series: Sequence[float]) -> list[float]:
    """Return a robust z-score per point, each against the whole series.

    Kept for the flat-series case and for tests. `trailing_zscores` is what `detect` uses.
    """
    if not series:
        return []

    centre = median(series)
    spread = _spread(series, centre)
    if spread == 0:
        return [0.0] * len(series)
    return [(v - centre) / spread for v in series]


def trailing_zscores(series: Sequence[float], window: int, warmup: int) -> list[float]:
    """Score each point against the `window` days that preceded it.

    Points inside the warmup return 0: with nothing to compare against, anything would be an
    anomaly, and a detector that fires on every tenant's first week is useless.
    """
    scores: list[float] = []
    for i, value in enumerate(series):
        if i < warmup:
            scores.append(0.0)
            continue

        history = series[max(0, i - window) : i]
        centre = median(history)
        spread = _spread(history, centre)
        if spread == 0:
            # A perfectly flat history: any increase is infinitely many sigmas, which is not
            # useful. Fall back to a proportional test so the score stays interpretable.
            scores.append(
                0.0
                if value <= centre
                else min(50.0, (value - centre) / max(centre, 1e-9))
            )
            continue
        scores.append((value - centre) / spread)
    return scores


def _spread(values: Sequence[float], centre: float) -> float:
    """Robust scale estimate, falling back to mean absolute deviation for a flat series."""
    if not values:
        return 0.0
    spread = mad(values, centre) * MAD_TO_SIGMA
    if spread > 0:
        return spread
    mean_abs = sum(abs(v - centre) for v in values) / len(values)
    return mean_abs * 1.2533  # E|X-mu| = sigma * sqrt(2/pi) for a normal


def detect(
    daily_spend: dict[str, dict[str, float]],
    *,
    threshold: float = DEFAULT_THRESHOLD,
    min_daily_usd: float = DEFAULT_MIN_DAILY_USD,
    min_history_days: int = MIN_HISTORY_DAYS,
    window_days: int = DEFAULT_WINDOW_DAYS,
    warmup_days: int = MIN_WARMUP_DAYS,
    min_multiple: float = DEFAULT_MIN_MULTIPLE,
) -> list[Anomaly]:
    """Find runaway workloads.

    `daily_spend` maps tenant_id -> {day -> spend}. Returns the flagged tenants, worst first.
    """
    total_spend = sum(sum(days.values()) for days in daily_spend.values())
    anomalies: list[Anomaly] = []

    for tenant_id, days in daily_spend.items():
        if len(days) < min_history_days:
            continue

        ordered_days = sorted(days)
        series = [days[d] for d in ordered_days]
        scores = trailing_zscores(series, window_days, warmup_days)

        flagged = []
        for i, (day, spend, score) in enumerate(
            zip(ordered_days, series, scores, strict=True)
        ):
            if score < threshold or spend < min_daily_usd:
                continue
            baseline = median(series[max(0, i - window_days) : i])
            # A day is only interesting if it is both statistically unusual AND materially
            # bigger. Either test alone produces a useless alert list.
            if baseline > 0 and spend < baseline * min_multiple:
                continue
            flagged.append((day, spend, score))
        if not flagged:
            continue

        tenant_total = sum(series)
        anomalies.append(
            Anomaly(
                tenant_id=tenant_id,
                # The baseline reported is the pre-onset level, not the whole-month median:
                # "it was $3/day and became $40/day" is the sentence an operator needs.
                baseline_usd=_baseline_before(series, ordered_days, flagged[0][0]),
                peak_usd=max(spend for _, spend, _ in flagged),
                total_usd=tenant_total,
                share_of_spend_pct=(tenant_total / total_spend * 100)
                if total_spend
                else 0.0,
                max_zscore=max(score for _, _, score in flagged),
                anomalous_days=[day for day, _, _ in flagged],
                onset_day=flagged[0][0],
            )
        )

    # Ranked by money, not by z-score. A tenant with a spectacular z-score on a $2 baseline is
    # less worth waking someone for than a moderate one on a $2,000 baseline.
    anomalies.sort(key=lambda a: -a.total_usd)
    return anomalies


def _baseline_before(series: Sequence[float], days: Sequence[str], onset: str) -> float:
    """Median daily spend before the first flagged day."""
    try:
        idx = list(days).index(onset)
    except ValueError:  # pragma: no cover
        return median(series)
    history = series[:idx]
    return median(history) if history else median(series)
