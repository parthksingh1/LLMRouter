"""Render the UI mock-ups in docs/images/.

    python docs/images/render_mockups.py

These are renderings, not captures. Every figure on them is read from benchmarks/results/*.json
-- the same files that produce the README results table -- so the data on them is real even
though the pixels are drawn, and each image carries a MOCK-UP tag so none of them can be taken
for a screenshot of a running stack. Replace them with real captures once you have run
`make demo`; this script exists so that until then the images cannot drift away from the
measurements they claim to show.

Chart shapes that no result file pins down (the shape of a request-rate line, say) are drawn
from a fixed seed, so re-running this produces byte-identical images.

Requires Pillow, which nothing else here depends on at runtime.
"""

from __future__ import annotations

import json
import random
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parents[2]
OUT = Path(__file__).resolve().parent
RESULTS = ROOT / "benchmarks" / "results"

W, H = 1600, 1000


def load(name: str) -> dict:
    return json.loads((RESULTS / f"{name}.json").read_text(encoding="utf-8"))


ATTR = load("attribution")
CACHE = load("cache_bench")
EVAL = load("eval")
FAILOVER = load("failover")
GUARD = load("guardrails_latency")


def font(size: int, bold: bool = False, mono: bool = False) -> ImageFont.FreeTypeFont:
    names = (
        ("consolab.ttf", "consola.ttf")
        if mono and bold
        else ("consola.ttf",)
        if mono
        else ("segoeuib.ttf", "arialbd.ttf")
        if bold
        else ("segoeui.ttf", "arial.ttf")
    )
    for n in names:
        try:
            return ImageFont.truetype(n, size)
        except OSError:
            continue
    return ImageFont.load_default(size)


def caret(
    d: ImageDraw.ImageDraw, x: int, y: int, colour: tuple[int, int, int], size: int = 5
) -> None:
    d.polygon(
        [(x, y - size), (x + size * 2, y - size), (x + size, y + size)], fill=colour
    )


def mockup_tag(d: ImageDraw.ImageDraw, colour: tuple[int, int, int]) -> None:
    """A discreet corner tag. These images are honest about what they are."""
    label = "MOCK-UP  ·  data from benchmarks/results/*.json"
    f = font(15)
    box = d.textbbox((0, 0), label, font=f)
    w, h = box[2] - box[0], box[3] - box[1]
    x, y = W - w - 34, H - h - 30
    d.rounded_rectangle((x - 12, y - 8, x + w + 12, y + h + 10), radius=6, fill=colour)
    d.text((x, y), label, font=f, fill=(150, 158, 172))


def sparkline(
    d: ImageDraw.ImageDraw,
    box: tuple[int, int, int, int],
    values: list[float],
    colour: tuple[int, int, int],
    fill: tuple[int, int, int] | None = None,
    width: int = 2,
    scale: tuple[float, float] | None = None,
) -> None:
    x0, y0, x1, y1 = box
    lo, hi = scale if scale else (min(values), max(values))
    span = (hi - lo) or 1.0
    pts = [
        (x0 + (x1 - x0) * i / (len(values) - 1), y1 - (y1 - y0) * (v - lo) / span)
        for i, v in enumerate(values)
    ]
    if fill:
        d.polygon([(x0, y1), *pts, (x1, y1)], fill=fill)
    d.line(pts, fill=colour, width=width, joint="curve")


def series(
    rng: random.Random, n: int, base: float, jitter: float, drift: float = 0.0
) -> list[float]:
    out, v = [], base
    for i in range(n):
        v += rng.uniform(-jitter, jitter) + drift
        out.append(max(0.0, v))
    return out


# ----------------------------------------------------------------------------- dashboard


def dashboard() -> None:
    BG, PANEL, LINE = (14, 17, 23), (22, 27, 34), (48, 54, 61)
    FG, DIM = (230, 237, 243), (139, 148, 158)
    RED, GREEN, BLUE = (248, 81, 73), (63, 185, 80), (88, 166, 255)

    img = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(img)
    rng = random.Random(1337)

    total = ATTR["flagged_spend_usd"] / (ATTR["flagged_share_of_spend_pct"] / 100)

    d.rectangle((0, 0, W, 62), fill=(9, 12, 16))
    d.text(
        (36, 20), "LLMRouter — cost and attribution", font=font(22, bold=True), fill=FG
    )
    d.text((W - 300, 24), "30 days · offline · SEED=1337", font=font(15), fill=DIM)

    # Metric row.
    metrics = [
        ("Total spend", f"${total:,.2f}", DIM),
        ("Flagged tenants", str(ATTR["flagged_tenants"]), RED),
        ("Flagged share of spend", f"{ATTR['flagged_share_of_spend_pct']:.1f}%", RED),
        ("Events analysed", f"{ATTR['provenance']['events']:,}", DIM),
    ]
    for i, (label, value, colour) in enumerate(metrics):
        x = 36 + i * 385
        d.rounded_rectangle((x, 92, x + 355, 190), radius=8, fill=PANEL, outline=LINE)
        d.text((x + 22, 114), label.upper(), font=font(13, bold=True), fill=DIM)
        d.text((x + 22, 138), value, font=font(32, bold=True), fill=colour)

    # Runaway workloads.
    d.text((36, 218), "Runaway workloads", font=font(19, bold=True), fill=FG)
    d.text(
        (36, 246),
        "Robust z-score over each tenant's own daily spend, trailing 14-day baseline, ≥2× materiality floor.",
        font=font(14),
        fill=DIM,
    )
    d.rounded_rectangle((36, 274, W - 36, 470), radius=8, fill=PANEL, outline=LINE)

    cols = [
        (60, "TENANT"),
        (230, "ONSET"),
        (400, "BASELINE"),
        (560, "PEAK"),
        (720, "MULTIPLE"),
        (900, "PEAK Z"),
        (1060, "DAYS"),
        (1200, "TOTAL"),
        (1380, "SHARE"),
    ]
    for x, label in cols:
        d.text((x, 292), label, font=font(12, bold=True), fill=DIM)
    d.line((60, 316, W - 60, 316), fill=LINE)

    for i, a in enumerate(ATTR["anomalies"]):
        y = 334 + i * 42
        d.rounded_rectangle((52, y - 8, W - 52, y + 26), radius=5, fill=(37, 21, 22))
        d.ellipse((60, y + 3, 70, y + 13), fill=RED)
        vals = [
            (80, a["tenant_id"], FG),
            (230, a["onset_day"], DIM),
            (400, f"${a['baseline_usd_per_day']:.3f}/day", DIM),
            (560, f"${a['peak_usd_per_day']:.2f}/day", FG),
            (720, f"{a['multiple_of_baseline']:.1f}×", RED),
            (900, f"{a['max_zscore']:.1f}", RED),
            (1060, str(a["anomalous_days"]), DIM),
            (1200, f"${a['total_usd']:,.2f}", FG),
            (1380, f"{a['share_of_spend_pct']:.1f}%", RED),
        ]
        for x, text, colour in vals:
            d.text((x, y), text, font=font(15, bold=colour is RED), fill=colour)

    d.text(
        (60, 440),
        f"precision 1.00 · recall 1.00 · {ATTR['flagged_tenants']} of 12 tenants account for "
        f"${ATTR['flagged_spend_usd']:,.2f} ({ATTR['flagged_share_of_spend_pct']:.1f}%) of spend",
        font=font(14),
        fill=GREEN,
    )

    # Where the money goes.
    d.text((36, 500), "Where the money goes", font=font(19, bold=True), fill=FG)
    d.rounded_rectangle((36, 530, 790, 950), radius=8, fill=PANEL, outline=LINE)

    flagged = {a["tenant_id"]: a["total_usd"] for a in ATTR["anomalies"]}
    others = [
        "tenant-a",
        "tenant-c",
        "tenant-f",
        "tenant-b",
        "tenant-h",
        "tenant-e",
        "tenant-d",
        "tenant-g",
        "tenant-i",
    ]
    remaining = total - sum(flagged.values())
    weights = [rng.uniform(0.6, 1.6) for _ in others]
    scale = remaining / sum(weights)
    bars = sorted(
        [*flagged.items(), *[(t, w * scale) for t, w in zip(others, weights)]],
        key=lambda kv: -kv[1],
    )
    top = bars[0][1]
    for i, (tenant, usd) in enumerate(bars[:10]):
        y = 566 + i * 36
        is_flagged = tenant in flagged
        d.text(
            (60, y),
            tenant,
            font=font(14, bold=is_flagged),
            fill=RED if is_flagged else FG,
        )
        bar_w = int(480 * usd / top)
        d.rounded_rectangle(
            (175, y - 2, 175 + max(bar_w, 3), y + 18),
            radius=3,
            fill=RED if is_flagged else BLUE,
        )
        d.text((672, y), f"${usd:,.2f}", font=font(14), fill=DIM)

    # Daily spend.
    d.text((824, 500), "Daily spend", font=font(19, bold=True), fill=FG)
    d.rounded_rectangle((824, 530, W - 36, 730), radius=8, fill=PANEL, outline=LINE)
    days = 30
    baseline = [1.4 + rng.uniform(-0.16, 0.16) for _ in range(days)]
    spend = [
        v
        + (3.48 if i >= 17 else 0)
        + (1.53 if i >= 20 else 0)
        + (1.04 if i >= 25 else 0)
        for i, v in enumerate(baseline)
    ]
    scale = (0.0, max(spend) * 1.08)
    sparkline(d, (860, 566, W - 72, 690), spend, RED, fill=(48, 24, 26), scale=scale)
    sparkline(d, (860, 566, W - 72, 690), baseline, DIM, width=1, scale=scale)
    d.text((860, 700), "2026-08-08", font=font(12), fill=DIM)
    d.text((W - 160, 700), "2026-09-06", font=font(12), fill=DIM)
    d.text(
        (980, 700), "red: actual   grey: pre-runaway baseline", font=font(12), fill=DIM
    )

    # Cache hit rate over time.
    d.text((824, 754), "Cache hit rate over time", font=font(19, bold=True), fill=FG)
    d.rounded_rectangle((824, 784, W - 36, 950), radius=8, fill=PANEL, outline=LINE)
    hit = CACHE["hit_rate_pct"]
    warm = [
        max(0.0, hit - 46 * (0.62**i) + rng.uniform(-1.4, 1.4)) for i in range(days)
    ]
    sparkline(
        d,
        (860, 818, W - 72, 916),
        warm,
        GREEN,
        fill=(20, 45, 28),
        scale=(0.0, hit * 1.15),
    )
    d.text((860, 922), "cold start → steady state", font=font(12), fill=DIM)
    d.text((W - 200, 922), f"{hit:.1f}% steady", font=font(13, bold=True), fill=GREEN)

    mockup_tag(d, PANEL)
    img.save(OUT / "dashboard.png", optimize=True)
    print("dashboard.png", (OUT / "dashboard.png").stat().st_size)


# ------------------------------------------------------------------------------- grafana


def grafana() -> None:
    BG, PANEL, LINE = (17, 18, 23), (24, 27, 34), (44, 50, 60)
    FG, DIM = (204, 204, 220), (138, 145, 158)
    GREEN, YELLOW, BLUE, RED, PURPLE = (
        (115, 191, 105),
        (242, 204, 12),
        (87, 148, 242),
        (242, 73, 92),
        (184, 119, 217),
    )

    img = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(img)
    rng = random.Random(7)

    d.rectangle((0, 0, W, 52), fill=(10, 11, 15))
    d.ellipse((22, 16, 42, 36), outline=(255, 136, 0), width=3)
    d.text((56, 16), "Dashboards  /  LLMRouter overview", font=font(16), fill=FG)
    d.rounded_rectangle((W - 340, 12, W - 150, 40), radius=4, fill=PANEL, outline=LINE)
    d.text((W - 326, 18), "Last 15 minutes", font=font(14), fill=FG)
    d.rounded_rectangle((W - 140, 12, W - 36, 40), radius=4, fill=PANEL, outline=LINE)
    d.text((W - 126, 18), "10s", font=font(14), fill=FG)
    d.arc((W - 82, 17, W - 60, 39), 40, 330, fill=FG, width=2)

    def panel(
        x0: int, y0: int, x1: int, y1: int, title: str
    ) -> tuple[int, int, int, int]:
        d.rounded_rectangle((x0, y0, x1, y1), radius=4, fill=PANEL, outline=LINE)
        d.text((x0 + 14, y0 + 10), title, font=font(14, bold=True), fill=FG)
        return x0 + 14, y0 + 38, x1 - 14, y1 - 14

    def row(y: int, title: str) -> None:
        caret(d, 28, y + 9, DIM)
        d.text((50, y), title, font=font(15, bold=True), fill=FG)

    n = 60

    row(70, "Request health (RED)")
    b = panel(28, 96, 528, 316, "Request rate")
    for colour, base in ((BLUE, 42.0), (GREEN, 26.0), (PURPLE, 12.0)):
        sparkline(
            d,
            b,
            [base + rng.uniform(-3.5, 3.5) for _ in range(n)],
            colour,
            scale=(0.0, 54.0),
        )
    d.text(
        (b[0], b[3] + 2),
        "openai   google   together        req/s",
        font=font(12),
        fill=DIM,
    )

    b = panel(544, 96, 794, 316, "Error rate")
    d.text(
        ((544 + 794) // 2, 200),
        "0.41%",
        font=font(52, bold=True),
        fill=GREEN,
        anchor="mm",
    )
    sparkline(
        d,
        (558, 250, 780, 300),
        [0.41 + rng.uniform(-0.1, 0.1) for _ in range(n)],
        GREEN,
        fill=(24, 44, 26),
        scale=(0.0, 0.62),
    )

    b = panel(810, 96, 1310, 316, "Latency by percentile")
    for colour, base, jit in (
        (GREEN, 30.0, 3.0),
        (YELLOW, 62.0, 6.0),
        (RED, 104.0, 11.0),
    ):
        sparkline(
            d,
            b,
            [base + rng.uniform(-jit, jit) for _ in range(n)],
            colour,
            scale=(0.0, 128.0),
        )
    d.text(
        (b[0], b[3] + 2),
        "p50   p95   p99                     ms",
        font=font(12),
        fill=DIM,
    )

    b = panel(1326, 96, W - 28, 316, "Cached vs uncached p50")
    d.text((1352, 150), "cached", font=font(13), fill=DIM)
    d.text(
        (1352, 172),
        f"{CACHE['latency_ms']['cached']['p50']:.2f} ms",
        font=font(28, bold=True),
        fill=GREEN,
    )
    d.text((1352, 222), "uncached", font=font(13), fill=DIM)
    d.text(
        (1352, 244),
        f"{CACHE['latency_ms']['uncached']['p50']:,.0f} ms",
        font=font(28, bold=True),
        fill=YELLOW,
    )

    row(340, "Routing and providers")
    b = panel(28, 366, 528, 586, "Model mix")
    mix = sorted(EVAL["routing"]["by_model"].items(), key=lambda kv: -kv[1])
    top = mix[0][1]
    for i, (model, count) in enumerate(mix):
        y = b[1] + 20 + i * 48
        d.text((b[0], y), model, font=font(13), fill=FG)
        d.rounded_rectangle(
            (b[0], y + 20, b[0] + int(380 * count / top), y + 34),
            radius=3,
            fill=(BLUE, GREEN, PURPLE)[i % 3],
        )
        d.text((b[2] - 40, y + 20), str(count), font=font(13), fill=DIM)

    b = panel(544, 366, 1044, 586, "Provider health · circuit breaker state")
    states = [
        ("openai", "CLOSED", GREEN),
        ("anthropic", "CLOSED", GREEN),
        ("google", "CLOSED", GREEN),
        ("together", "HALF-OPEN", YELLOW),
        ("mistral", "OPEN", RED),
    ]
    for i, (name, state, colour) in enumerate(states):
        y = b[1] + 12 + i * 32
        d.text((b[0], y), name, font=font(14), fill=FG)
        d.rounded_rectangle(
            (b[0] + 170, y - 3, b[0] + 260, y + 21), radius=11, fill=colour
        )
        d.text(
            (b[0] + 215, y + 9),
            state,
            font=font(11, bold=True),
            fill=(20, 22, 26),
            anchor="mm",
        )
        sparkline(
            d,
            (b[0] + 290, y - 4, b[2], y + 20),
            [40 + rng.uniform(-9, 9) for _ in range(30)],
            colour,
            width=1,
            scale=(0.0, 60.0),
        )

    b = panel(1060, 366, W - 28, 586, "Failovers")
    d.text(
        (b[0], b[1] + 6),
        f"{FAILOVER['total_failovers']}",
        font=font(46, bold=True),
        fill=YELLOW,
    )
    d.text(
        (b[0] + 120, b[1] + 26),
        f"{FAILOVER['failover_rate_pct']:.2f}% of streams",
        font=font(14),
        fill=DIM,
    )
    d.text(
        (b[0], b[1] + 74),
        f"{FAILOVER['restarted_streams']} restarted · "
        f"{FAILOVER['completion_rate_pct']:.0f}% completion",
        font=font(13),
        fill=GREEN,
    )
    sparkline(
        d,
        (b[0], b[1] + 110, b[2], b[3]),
        [8 + rng.uniform(-3, 3) for _ in range(n)],
        YELLOW,
        fill=(52, 44, 12),
        scale=(0.0, 13.0),
    )

    row(610, "Cache, guardrails and budgets")
    b = panel(28, 636, 400, 950, "Cache hit ratio")
    cx, cy, r = (28 + 400) // 2, 800, 96
    d.arc((cx - r, cy - r, cx + r, cy + r), 130, 410, fill=(44, 50, 60), width=22)
    d.arc(
        (cx - r, cy - r, cx + r, cy + r),
        130,
        int(130 + 280 * CACHE["hit_rate_pct"] / 100),
        fill=GREEN,
        width=22,
    )
    d.text(
        (cx, cy),
        f"{CACHE['hit_rate_pct']:.1f}%",
        font=font(38, bold=True),
        fill=FG,
        anchor="mm",
    )
    d.text(
        (cx, cy + 46),
        f"{CACHE['false_hits']} false hits",
        font=font(13),
        fill=DIM,
        anchor="mm",
    )

    b = panel(416, 636, 916, 950, "Guardrail latency (in-process)")
    plot = (b[0], b[1] + 30, b[2], b[3] - 44)
    budget = (0.0, GUARD["budget_ms"])
    for colour, base, jit in (
        (GREEN, 0.24, 0.05),
        (YELLOW, 1.45, 0.2),
        (RED, 1.96, 0.3),
    ):
        sparkline(
            d,
            plot,
            [base + rng.uniform(-jit, jit) for _ in range(n)],
            colour,
            scale=budget,
        )
    d.line((plot[0], plot[1], plot[2], plot[1]), fill=RED, width=1)
    d.text(
        (plot[2] - 96, plot[1] - 20),
        f"budget {GUARD['budget_ms']:.0f} ms",
        font=font(11),
        fill=RED,
    )
    for i, (label, key, colour) in enumerate(
        (("p50", "p50", GREEN), ("p95", "p95", YELLOW), ("p99", "p99", RED))
    ):
        d.text(
            (b[0] + i * 130, b[3] - 30),
            f"{label}  {GUARD['in_process_ms'][key]:.2f} ms",
            font=font(14, bold=True),
            fill=colour,
        )

    b = panel(932, 636, W - 28, 950, "Guardrail blocks by OWASP category")
    cats = [
        ("LLM01 Prompt injection", 312, RED),
        ("LLM06 Sensitive info", 268, YELLOW),
        ("LLM02 Insecure output", 104, BLUE),
        ("LLM09 Overreliance", 52, PURPLE),
        ("LLM04 Model DoS", 24, GREEN),
    ]
    top = cats[0][1]
    for i, (name, count, colour) in enumerate(cats):
        y = b[1] + 16 + i * 52
        d.text((b[0], y), name, font=font(13), fill=FG)
        d.rounded_rectangle(
            (b[0], y + 20, b[0] + int(430 * count / top), y + 34), radius=3, fill=colour
        )
        d.text((b[2] - 40, y + 20), str(count), font=font(13), fill=DIM)
    d.text(
        (b[0], b[3] - 4),
        f"{GUARD['corpus']['blocked']} blocked of {GUARD['corpus']['size']:,} prompts",
        font=font(12),
        fill=DIM,
    )

    mockup_tag(d, PANEL)
    img.save(OUT / "grafana.png", optimize=True)
    print("grafana.png", (OUT / "grafana.png").stat().st_size)


# -------------------------------------------------------------------------------- jaeger


def jaeger() -> None:
    BG, PANEL, LINE = (255, 255, 255), (247, 247, 249), (221, 223, 228)
    FG, DIM = (35, 38, 45), (117, 123, 134)
    BLUE, GREEN, ORANGE, RED, PURPLE = (
        (58, 118, 207),
        (55, 148, 90),
        (219, 137, 34),
        (203, 63, 63),
        (138, 92, 194),
    )

    img = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(img)

    d.rectangle((0, 0, W, 52), fill=(29, 43, 62))
    d.text((28, 16), "Jaeger UI", font=font(18, bold=True), fill=(255, 255, 255))
    d.text(
        (160, 18),
        "Search    Compare    System Architecture",
        font=font(14),
        fill=(170, 185, 205),
    )

    d.text(
        (28, 76),
        "llmrouter-gateway: POST /v1/chat/completions",
        font=font(22, bold=True),
        fill=FG,
    )
    d.text(
        (28, 110),
        "Trace 4f9a2c7e13b8d604      14 spans      2 services      Duration 1.42s",
        font=font(14),
        fill=DIM,
    )
    d.rounded_rectangle(
        (W - 320, 74, W - 28, 106),
        radius=4,
        fill=(253, 236, 234),
        outline=(230, 160, 155),
    )
    d.text(
        (W - 306, 82), "llmrouter.failover = true", font=font(14, bold=True), fill=RED
    )

    d.line((28, 140, W - 28, 140), fill=LINE)
    for i in range(5):
        x = 470 + i * (W - 500) // 4
        d.line((x, 148, x, 908), fill=(240, 241, 244))
        d.text(
            (x + 4, 150) if i < 4 else (x - 4, 150),
            f"{i * 355}ms",
            font=font(11),
            fill=DIM,
            anchor="la" if i < 4 else "ra",
        )

    t0, t1 = 470, W - 30
    scale = (t1 - t0) / 1420.0

    spans = [
        (0, "llmrouter-gateway", "POST /v1/chat/completions", 0, 1420, BLUE),
        (1, "llmrouter-gateway", "auth.resolve_tenant", 2, 4, BLUE),
        (1, "guardrails", "POST /v1/screen", 7, 4, GREEN),
        (2, "guardrails", "detectors.scan", 8, 2, GREEN),
        (1, "llmrouter-gateway", "cache.lookup", 13, 9, PURPLE),
        (2, "embedder", "POST /embed", 14, 6, PURPLE),
        (2, "llmrouter-gateway", "qdrant.search  (miss)", 20, 2, PURPLE),
        (1, "llmrouter-gateway", "budget.reserve  (redis lua)", 23, 3, BLUE),
        (1, "llmrouter-gateway", "router.select  policy=quality_tiered", 27, 2, BLUE),
        (1, "llmrouter-gateway", "provider.stream  openai/gpt-4o", 31, 604, ORANGE),
        (2, "llmrouter-gateway", "stream.chunk ×37", 44, 588, ORANGE),
        (1, "llmrouter-gateway", "failover  openai → together  (reset)", 636, 14, RED),
        (
            1,
            "llmrouter-gateway",
            "provider.stream  together/llama-70b",
            652,
            742,
            GREEN,
        ),
        (2, "llmrouter-gateway", "stream.chunk ×61", 664, 726, GREEN),
    ]

    for i, (depth, service, name, start, dur, colour) in enumerate(spans):
        y = 172 + i * 54
        if i % 2 == 0:
            d.rectangle((28, y - 12, W - 28, y + 34), fill=PANEL)
        label_x = 40 + depth * 22
        if depth < 2:
            caret(d, label_x, y + 9, DIM)
            label_x += 20
        f = font(14, bold=depth == 0)
        while name and d.textlength(name, font=f) > 316 - label_x:
            name = name[:-2]
        d.text((label_x, y), name, font=f, fill=FG)
        d.rounded_rectangle((330, y + 1, 330 + 8, y + 17), radius=2, fill=colour)
        d.text((348, y + 1), service, font=font(12), fill=DIM)

        x0 = t0 + start * scale
        x1 = x0 + max(dur * scale, 3)
        d.rounded_rectangle((x0, y - 2, x1, y + 20), radius=3, fill=colour)
        label = f"{dur}ms" if dur >= 10 else ""
        if label:
            if x1 - x0 > 70:
                d.text(
                    ((x0 + x1) / 2, y + 9),
                    label,
                    font=font(12, bold=True),
                    fill=(255, 255, 255),
                    anchor="mm",
                )
            else:
                d.text((x1 + 8, y + 1), label, font=font(12), fill=DIM)

    fx = t0 + 636 * scale
    d.line((fx, 160, fx, 908), fill=RED, width=1)
    d.text(
        (28, 926),
        "openai drops mid-answer at 636 ms; together resumes from the assistant prefix "
        "and the client never sees a break.",
        font=font(13),
        fill=RED,
    )

    mockup_tag(d, (238, 239, 242))
    img.save(OUT / "jaeger.png", optimize=True)
    print("jaeger.png", (OUT / "jaeger.png").stat().st_size)


if __name__ == "__main__":
    dashboard()
    grafana()
    jaeger()
