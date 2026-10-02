#!/usr/bin/env python3
"""
latencyDiagramMaker.py
======================

Direct-comparison latency chart for a comparative-latency run -- the latency
twin of ecoDiagramMaker.py, built the same way so the two thesis figures can be
read side by side without caveats.

Phase A (Random) and Phase B (Latency) are each measured from THEIR OWN start
(elapsed minutes since that phase's first valid event) and plotted on the SAME
X axis, so "N minutes into the phase" compares directly between the two
policies -- e.g. "at minute 5, were Consumers closer to their provider under
Random or under Latency?".

Method: every Consumer carries its latest measured RTT to the provider it is
peered with, forward-filled until its next successful reservation; that
per-Consumer state is resampled onto a regular grid and averaged across
Consumers.

One deliberate difference: the Y axis is the MEAN RTT across active Consumers,
where the eco chart plots a SUM of carbon intensities. A sum of milliseconds has
no physical meaning, and it grows with the number of Consumers, so a 3x7 run and
a 30x70 run would land on scales a factor of ten apart. The mean stays in real
milliseconds on the same scale at every size. (The sum is still written to the
CSV output as raw data.)

Generic by design: the number of Consumers is auto-detected from the CSV, so
the same command works unchanged for every experiment size, e.g.:

    python3 federation-tests/scripts/latencyDiagramMaker.py --input results/3c-7p/reservations.csv
    python3 federation-tests/scripts/latencyDiagramMaker.py --input results/30c-70p/reservations.csv

Behind the two curves it shades an OBSERVED RANGE, the latency counterpart of
the achievable range in the eco chart: at every point in time, the mean of the
x fastest RTTs measured anywhere in the federation and the mean of the x
slowest, where x is the number of Consumers placing then.

"Observed", not "achievable": the simulated delays are never written to a CSV,
so only the RTTs someone actually measured can be ranked -- a Consumer probes
one provider per iteration under Random and its three nearest under Latency,
never all of them. The floor is therefore the best anybody happened to measure,
which is at or above the best there was, so the policy looks no better than it
is. The summary reports the coverage (measured pairs out of all of them); the
true optimum would need the harness to export its tc delay matrix.

Probes are grouped by refresh window -- the delay of a pair is redrawn once per
window and constant in between -- with the window boundaries estimated from the
data rather than assumed, since the refresh ticker starts after the policy wait,
not at minute zero. Getting that alignment wrong mixes two draws of the same
pair and drags the floor around.

Curve and range are therefore measured a few tens of seconds apart, so nothing
forces the curve inside the range; the summary reports how many points fall
outside (none, on the runs in this thesis).

Outputs (default: an `analysis/` directory next to the input file; override
with --output-dir):

    latency_comparison.png   -- 300 DPI step chart, both policies overlaid
    latency_comparison.pdf   -- vector version of the same chart
    latency_comparison.csv   -- the underlying per-phase timeline data
    latency_summary.md       -- text summary + sanity-check warnings

`reservations.csv` is the only required input. `probes.csv` (picked up
automatically from the same directory, or passed with --probes) adds the
observed range; without it the chart is drawn exactly as before.

Requires: Python 3, pandas, matplotlib (standard library otherwise).
"""

from __future__ import annotations

import argparse
import math
import sys
from pathlib import Path

import pandas as pd

import matplotlib

matplotlib.use("Agg")  # headless-safe: no display needed (e.g. on a remote server)
import matplotlib.pyplot as plt  # noqa: E402  (must follow matplotlib.use)

# Where the refresh boundaries fall is already solved, carefully, by the
# alignment checker next door: every pair is redrawn by one ticker, so each
# value change brackets the same boundary, and the brackets are averaged
# circularly. Importing it keeps one implementation of that reasoning.
from verifyLatencyReplayAlignment import estimate_grid_offset  # noqa: E402

import pgfplotsWriter  # noqa: E402  (shared LaTeX output; see its docstring)

# Columns this script expects to find in the input CSV. A comparative-latency
# reservations.csv always carries all of them (see ReservationRecord in
# federation-tests/testlib/writer.go).
REQUIRED_COLUMNS = [
    "timestamp",
    "consumer_id",
    "phase",
    "policy",
    "provider_id",
    "action",
    "rtt_ms",
    "outcome",
    "final_phase",
]

# The two phase labels this test harness always produces (testlib.PhaseA /
# testlib.PhaseB), which is what lets phase boundaries be read from the phase
# column instead of being passed in.
PHASE_A = "phase-a"
PHASE_B = "phase-b"

# Columns read from probes.csv, the optional second input that carries every
# RTT the Consumers measured, not just the one they ended up peering with (see
# WriteProbeCSV in federation-tests/testlib/writer.go).
PROBE_COLUMNS = [
    "timestamp",
    "consumer_id",
    "phase",
    "provider_id",
    "rtt_ms",
]


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="latencyDiagramMaker.py",
        description=(
            "Overlay Phase A (Random) and Phase B (Latency) on the SAME X axis -- "
            "each measured as elapsed minutes since ITS OWN start -- plotting the mean "
            "RTT from each Consumer to the provider it is peered with. Works unchanged "
            "for any experiment size -- the number of Consumers is auto-detected from "
            "the CSV."
        ),
        epilog=(
            "examples (identical command, only --input changes with scale):\n"
            "  python3 federation-tests/scripts/latencyDiagramMaker.py --input results/3c-7p/reservations.csv\n"
            "  python3 federation-tests/scripts/latencyDiagramMaker.py --input results/30c-70p/reservations.csv\n"
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "--input",
        required=True,
        type=Path,
        help="Path to a comparative-latency reservations.csv file.",
    )
    parser.add_argument(
        "--output-dir",
        type=Path,
        default=None,
        help="Output directory (default: an 'analysis' folder next to --input).",
    )
    parser.add_argument(
        "--grid",
        choices=["regular", "events"],
        default="regular",
        help=(
            "How to build each phase's own timeline. 'regular' (default) resamples "
            "onto an evenly spaced grid (see --grid-minutes); 'events' uses every "
            "distinct valid event timestamp instead, giving an exact step function "
            "at the cost of a less even spacing."
        ),
    )
    parser.add_argument(
        "--grid-minutes",
        type=float,
        default=1.0,
        help="Grid spacing in minutes when --grid=regular (default: 1.0).",
    )
    parser.add_argument(
        "--probes",
        type=Path,
        default=None,
        help=(
            "Path to the run's probes.csv, used to shade the observed RTT "
            "range (default: probes.csv next to --input; the range is simply "
            "left out if that file is not there)."
        ),
    )
    parser.add_argument(
        "--range-window-minutes",
        type=float,
        default=2.0,
        help=(
            "Window the probes are grouped into for the observed range, i.e. how "
            "often the harness redraws its tc delays (latencyRefreshInterval, 2m "
            "in every shipped config -- default: 2.0). A pair's delay is constant "
            "inside one window, so every probe in it measures the same condition; "
            "grouping any finer just splits the same measurements into windows that "
            "miss some pairs."
        ),
    )
    parser.add_argument(
        "--no-range",
        action="store_true",
        help="Draw only the two policy curves, even if probes.csv is available.",
    )
    args = parser.parse_args(argv)
    if args.grid == "regular" and args.grid_minutes <= 0:
        parser.error("--grid-minutes must be a positive number")
    if args.range_window_minutes <= 0:
        parser.error("--range-window-minutes must be a positive number")
    return args


def load_reservations(path: Path) -> pd.DataFrame:
    """Reads and validates the input CSV, exiting with a clear message on any
    problem -- this script is run by hand while writing a thesis, so a
    traceback is the wrong way to say "wrong file"."""
    if not path.is_file():
        sys.exit(f"error: input file not found: {path}")

    try:
        df = pd.read_csv(path, dtype=str)
    except Exception as exc:  # noqa: BLE001 -- surfaced to the user as-is
        sys.exit(f"error: failed to read CSV '{path}': {exc}")

    if df.empty:
        sys.exit(f"error: input CSV '{path}' has no data rows.")

    missing = [c for c in REQUIRED_COLUMNS if c not in df.columns]
    if missing:
        sys.exit(
            "error: input CSV is missing required column(s): "
            + ", ".join(missing)
            + f"\n  found columns: {', '.join(df.columns)}"
            + "\n  this script expects a comparative-latency reservations.csv "
            "(see federation-tests/testlib/writer.go, ReservationRecord)."
        )

    # Go's time.RFC3339Nano trims trailing zero fractional digits, so the
    # precision varies row to row; pd.to_datetime handles that, and
    # errors="coerce" drops a stray bad row instead of aborting the run.
    df["timestamp"] = pd.to_datetime(df["timestamp"], utc=True, errors="coerce")
    n_bad_ts = int(df["timestamp"].isna().sum())
    if n_bad_ts:
        print(
            f"warning: {n_bad_ts} row(s) had an unparseable timestamp and were dropped",
            file=sys.stderr,
        )
        df = df.dropna(subset=["timestamp"])
    if df.empty:
        sys.exit("error: no rows with a parseable timestamp remain in the input.")

    # "+Inf" (unreachable provider) parses to inf here; "0.000" stays 0. Both
    # are rejected by filter_valid, not here, so they can still be counted.
    df["rtt_ms"] = pd.to_numeric(df["rtt_ms"], errors="coerce")
    return df


def has_rtt_reading(rtt: pd.Series) -> pd.Series:
    """True where rtt_ms is an actual measurement.

    The writer formats RTT unconditionally, so a row that never measured one
    (a keep with no growable alternative) comes out as 0.000, and an
    unreachable provider as +Inf. Neither is a latency: a 0 would enter the
    mean as a perfect connection and drag it down, so both count as no reading.
    """
    return rtt.notna() & rtt.map(lambda v: math.isfinite(v) if pd.notna(v) else False) & (rtt > 0)


def is_eco_csv(df: pd.DataFrame) -> bool:
    """A comparative-eco reservations.csv shares the same header -- rtt_ms is
    present but never filled -- so the column check alone cannot tell the two
    apart. What does is that not a single row carries a real RTT."""
    return not has_rtt_reading(df["rtt_ms"]).any()


def filter_valid(df: pd.DataFrame) -> pd.DataFrame:
    """Rows that may update a Consumer's active RTT state: a successful,
    Peered reservation with a real RTT reading and a real consumer_id.
    Everything else never touches the per-Consumer state, so a failed or
    unmeasured event simply leaves the previous reading in place."""
    consumer_id = df["consumer_id"].fillna("")
    mask = (
        (df["outcome"] == "success")
        & (df["final_phase"] == "Peered")
        & has_rtt_reading(df["rtt_ms"])
        & df["consumer_id"].notna()
        & (consumer_id.str.strip() != "")
    )
    return df[mask].copy()


def phase_duration(df: pd.DataFrame, phase_value: str) -> pd.Timedelta | None:
    """Wall-clock span of a phase, from the RAW (unfiltered) data."""
    ts = df.loc[df["phase"] == phase_value, "timestamp"]
    if ts.empty:
        return None
    return ts.max() - ts.min()


def phase_policy_name(df: pd.DataFrame, phase_value: str, default: str) -> str:
    """The policy name recorded for a phase, read from the data so labels stay
    correct if policy names ever change."""
    values = df.loc[df["phase"] == phase_value, "policy"].dropna()
    values = values[values.str.strip() != ""]
    return values.iloc[0] if not values.empty else default


def build_state_matrix(valid: pd.DataFrame) -> pd.DataFrame:
    """Wide matrix: index = distinct valid-event timestamp, one column per
    Consumer, values = that Consumer's latest RTT as of that timestamp.
    Forward-filled per column, and NaN (not zero) before a Consumer's first
    reading, so a Consumer not yet peered is absent rather than instant."""
    v = valid.sort_values("timestamp")
    # pivot() raises on duplicate (index, column) pairs; keep the later event.
    v = v.drop_duplicates(subset=["timestamp", "consumer_id"], keep="last")
    wide = v.pivot(index="timestamp", columns="consumer_id", values="rtt_ms")
    return wide.sort_index().ffill()


def build_timeline_index(wide: pd.DataFrame, grid: str, grid_minutes: float) -> pd.DatetimeIndex:
    if grid == "events":
        return wide.index
    t0, t1 = wide.index.min(), wide.index.max()
    idx = pd.date_range(t0, t1, freq=pd.Timedelta(minutes=grid_minutes))
    if idx.empty or idx[-1] < t1:
        idx = idx.append(pd.DatetimeIndex([t1]))
    return idx


def resample_state(wide: pd.DataFrame, target_index: pd.DatetimeIndex) -> pd.DataFrame:
    """Evaluates the per-Consumer state at target_index. A plain reindex only
    aligns on exact timestamp matches; unioning the event index in first, then
    forward-filling, carries each Consumer's last reading onto every grid point
    while leaving not-yet-active Consumers as NaN."""
    unioned = wide.reindex(wide.index.union(target_index)).sort_index().ffill()
    return unioned.reindex(target_index)


def compute_aggregate(state: pd.DataFrame) -> pd.DataFrame:
    active_consumers = state.notna().sum(axis=1)
    total = state.sum(axis=1, skipna=True)
    # Zero active Consumers (before anyone's first reading) leaves the mean
    # blank rather than inf; plain NaN, not pd.NA, to keep float64 arithmetic.
    denom = active_consumers.astype("float64").replace(0.0, float("nan"))
    mean = total / denom
    return pd.DataFrame(
        {
            "timestamp": state.index,
            "active_consumers": active_consumers.to_numpy(),
            "mean_rtt_ms": mean.to_numpy(),
            "sum_rtt_ms": total.to_numpy(),
        }
    )


def build_phase_local_timeline(
    valid: pd.DataFrame,
    phase_value: str,
    policy_label: str,
    grid: str,
    grid_minutes: float,
) -> pd.DataFrame | None:
    """One phase's own timeline, measured from THAT PHASE's first valid event
    (elapsed_minutes == 0 there), which is what makes the two curves overlay
    instead of sitting end to end. None if the phase has no valid data."""
    phase_valid = valid[valid["phase"] == phase_value]
    if phase_valid.empty:
        return None

    wide = build_state_matrix(phase_valid)
    t0_phase = wide.index.min()
    target_index = build_timeline_index(wide, grid, grid_minutes)
    state = resample_state(wide, target_index)

    timeline = compute_aggregate(state)
    timeline["elapsed_minutes"] = (timeline["timestamp"] - t0_phase).dt.total_seconds() / 60.0
    timeline["phase"] = phase_value
    timeline["policy"] = policy_label
    return timeline[
        [
            "timestamp",
            "elapsed_minutes",
            "phase",
            "policy",
            "active_consumers",
            "mean_rtt_ms",
            "sum_rtt_ms",
        ]
    ]


def load_probes(path: Path) -> pd.DataFrame | None:
    """Reads probes.csv, every RTT the Consumers measured.

    Returns None -- with a warning, never an error -- when the file is absent
    or unusable: the observed range is an addition to the chart, and a run from
    before this file existed must still plot."""
    if not path.is_file():
        print(
            f"warning: {path} not found; the chart will show the two policy "
            "curves without the observed RTT range.",
            file=sys.stderr,
        )
        return None

    try:
        df = pd.read_csv(path, dtype=str)
    except Exception as exc:  # noqa: BLE001 -- surfaced to the user as-is
        print(f"warning: failed to read '{path}' ({exc}); skipping the observed range.", file=sys.stderr)
        return None

    missing = [c for c in PROBE_COLUMNS if c not in df.columns]
    if missing:
        print(
            f"warning: '{path}' is missing column(s) {', '.join(missing)}; skipping the observed range.",
            file=sys.stderr,
        )
        return None

    df["timestamp"] = pd.to_datetime(df["timestamp"], utc=True, errors="coerce")
    df["rtt_ms"] = pd.to_numeric(df["rtt_ms"], errors="coerce")
    # A zero RTT is the writer's placeholder for "no measurement", the same
    # convention has_rtt_reading() applies to reservations.csv.
    keep = (
        df["timestamp"].notna()
        & df["rtt_ms"].notna()
        & (df["rtt_ms"] > 0)
        & df["consumer_id"].notna()
        & df["provider_id"].notna()
    )
    df = df[keep]
    if df.empty:
        print(f"warning: '{path}' has no usable RTT readings; skipping the observed range.", file=sys.stderr)
        return None
    return df


# An offset is trusted when enough value changes agree on it. Under Random a
# pair is probed once and then dropped, so that phase rarely brackets anything
# and borrows the policy phase's estimate -- the two phases run the same
# schedule, which is what the replay is built on.
MIN_OFFSET_BRACKETS = 20
MIN_OFFSET_CONFIDENCE = 0.5


def probe_rows(probes: pd.DataFrame, phase_value: str, t0: pd.Timestamp) -> pd.DataFrame:
    """One phase's probes, stamped with minutes elapsed since its start."""
    rows = probes[probes["phase"] == phase_value].copy()
    if rows.empty:
        return rows
    rows["elapsed_minutes"] = (rows["timestamp"] - t0).dt.total_seconds() / 60.0
    return rows[rows["elapsed_minutes"] >= 0].copy()


def estimate_window_offset(rows: pd.DataFrame, window_minutes: float) -> tuple[float, float, int]:
    """Where this phase's refresh boundaries fall, in minutes since its start.

    The boundaries are not at minute 0, 2, 4...: the refresh ticker starts
    after the policy wait and the prober-cache drain, so windows nailed to the
    first reservation straddle two real ones and mix two draws of the same
    pair -- which is exactly what made the floor cross the curve. Every pair is
    redrawn by one ticker, so each value change brackets the same boundary;
    estimate_grid_offset averages those brackets circularly.

    Only changes bracketed within half a window are used: a wider bracket may
    contain a boundary anywhere inside it and just blurs the estimate."""
    if rows.empty:
        return 0.0, 0.0, 0
    step = window_minutes if window_minutes > 0 else 1.0
    samples: dict[tuple[str, str], list[tuple[float, float]]] = {}
    for (consumer_id, provider_id), group in rows.sort_values("elapsed_minutes").groupby(
        ["consumer_id", "provider_id"]
    ):
        samples[(consumer_id, provider_id)] = list(
            zip(group["elapsed_minutes"] * 60.0, group["rtt_ms"])
        )
    # tolerance 1 ms: a pair re-measured under the same delay lands within a
    # fraction of a millisecond, a redraw moves it by tens.
    offset_s, resultant, used = estimate_grid_offset(
        samples, tick=step * 60.0, tolerance=1.0, max_bracket=step * 30.0
    )
    return offset_s / 60.0, resultant, used


def assign_windows(rows: pd.DataFrame, window_minutes: float, offset: float) -> pd.DataFrame:
    """Labels each probe with the refresh window it belongs to. Pooling by
    window is what lets the two phases be compared: the replay puts them in
    front of the same environment at the same elapsed time."""
    if rows.empty:
        return rows
    step = window_minutes if window_minutes > 0 else 1.0
    out = rows.copy()
    out["bin"] = (((out["elapsed_minutes"] - offset) // step) * step + offset).round(6)
    return out


def pool_by_window(binned: pd.DataFrame) -> dict[float, tuple[list[float], int]]:
    """Every RTT measured in each refresh window, sorted, with how many
    distinct Consumer-provider pairs it came from."""
    pools: dict[float, tuple[list[float], int]] = {}
    for bin_value, group in binned.groupby("bin"):
        values = sorted(float(v) for v in group["rtt_ms"].to_numpy())
        pairs = int(group.groupby(["consumer_id", "provider_id"]).ngroups)
        pools[float(bin_value)] = (values, pairs)
    return pools


def observed_range(
    pools: dict[float, tuple[list[float], int]],
    timeline: pd.DataFrame,
    window_minutes: float,
    window_offset: float,
    pairs_possible: int,
) -> pd.DataFrame | None:
    """Mean of the x fastest and the x slowest RTTs measured in the window a
    point falls in, with x the number of Consumers that point averages.

    x has to be the point's own count, not the window's largest: these edges
    are means, so a floor computed over more values than the curve averages
    would sit above it. With the same x, and with every value the curve uses
    present in the pool, floor <= curve <= ceiling holds at every point.

    `coverage` says how much of the Consumer-provider matrix the pool covers:
    the lower it is, the more the floor is "the best anybody happened to
    measure" rather than the best there was."""
    if timeline is None or timeline.empty or not pools:
        return None

    step = window_minutes if window_minutes > 0 else 1.0
    records = []
    for elapsed, x in zip(timeline["elapsed_minutes"], timeline["active_consumers"]):
        key = round(((float(elapsed) - window_offset) // step) * step + window_offset, 6)
        pool = pools.get(key)
        if pool is None:
            continue
        values, pairs = pool
        take = min(int(x), len(values))
        if take <= 0:
            continue
        records.append(
            {
                "elapsed_minutes": float(elapsed),
                "floor": sum(values[:take]) / take,
                "ceiling": sum(values[-take:]) / take,
                "x_used": take,
                "observed_pairs": pairs,
                "coverage": (pairs / pairs_possible) if pairs_possible > 0 else float("nan"),
            }
        )
    if not records:
        return None
    return pd.DataFrame(records).sort_values("elapsed_minutes").reset_index(drop=True)


def merge_bands(band_a: pd.DataFrame | None, band_b: pd.DataFrame | None) -> pd.DataFrame | None:
    """One band out of the two phases: the replay puts both in front of the
    same environment, so their edges are averaged point by point on elapsed
    time; where only one phase reaches a minute, that phase's value is used."""
    parts = [b for b in (band_a, band_b) if b is not None and not b.empty]
    if not parts:
        return None
    joined = pd.concat(parts, ignore_index=True)
    joined["elapsed_minutes"] = joined["elapsed_minutes"].round(6)
    merged = (
        joined.groupby("elapsed_minutes", as_index=False)
        .agg(
            floor=("floor", "mean"),
            ceiling=("ceiling", "mean"),
            x_used=("x_used", "max"),
            observed_pairs=("observed_pairs", "max"),
            coverage=("coverage", "mean"),
        )
        .sort_values("elapsed_minutes")
        .reset_index(drop=True)
    )
    return merged.dropna(subset=["floor", "ceiling"])


def attach_band_columns(
    timeline_out: pd.DataFrame,
    band: pd.DataFrame | None,
    band_a: pd.DataFrame | None,
    band_b: pd.DataFrame | None,
    window_minutes: float,
) -> pd.DataFrame:
    """Adds the range to the exported timeline: the drawn band (both phases
    pooled) with its coverage, and next to it each phase's own edges."""
    out = timeline_out.copy()
    step = window_minutes if window_minutes > 0 else 1.0
    key = ((out["elapsed_minutes"] // step) * step).round(6)

    for column in (
        "band_floor",
        "band_ceiling",
        "band_observed_pairs",
        "band_coverage",
        "phase_band_floor",
        "phase_band_ceiling",
    ):
        out[column] = float("nan")
    if band is None or band.empty:
        return out

    drawn = band.drop_duplicates(subset="elapsed_minutes").set_index("elapsed_minutes")
    out["band_floor"] = key.map(drawn["floor"]).to_numpy()
    out["band_ceiling"] = key.map(drawn["ceiling"]).to_numpy()
    out["band_observed_pairs"] = key.map(drawn["observed_pairs"]).to_numpy()
    out["band_coverage"] = key.map(drawn["coverage"]).to_numpy()

    for phase_value, phase_band in ((PHASE_A, band_a), (PHASE_B, band_b)):
        if phase_band is None or phase_band.empty:
            continue
        own = phase_band.drop_duplicates(subset="elapsed_minutes").set_index("elapsed_minutes")
        rows = out["phase"] == phase_value
        out.loc[rows, "phase_band_floor"] = key[rows].map(own["floor"]).to_numpy()
        out.loc[rows, "phase_band_ceiling"] = key[rows].map(own["ceiling"]).to_numpy()
    return out


def make_comparison_chart(
    timeline_a: pd.DataFrame | None,
    timeline_b: pd.DataFrame | None,
    subtitle: str,
    phase_a_label: str,
    phase_b_label: str,
    png_out: Path,
    pdf_out: Path,
    band: pd.DataFrame | None = None,
) -> None:
    plt.rcParams.update(
        {
            "font.size": 11,
            "axes.grid": True,
            "grid.alpha": 0.35,
            "grid.linestyle": "--",
            "grid.linewidth": 0.6,
            "axes.spines.top": False,
            "axes.spines.right": False,
            "figure.facecolor": "white",
            "axes.facecolor": "white",
            "savefig.facecolor": "white",
        }
    )
    fig, ax = plt.subplots(figsize=(11, 5.5))

    # Drawn first and with a low zorder so the two policy curves stay readable
    # on top of it. Same shading as ecoDiagramMaker.py.
    if band is not None and not band.empty:
        ax.fill_between(
            band["elapsed_minutes"],
            band["floor"],
            band["ceiling"],
            step="post",
            color="#d9c37a",
            alpha=0.28,
            linewidth=0,
            zorder=0,
            label="Observed range",
        )
        for edge in ("floor", "ceiling"):
            ax.step(
                band["elapsed_minutes"],
                band[edge],
                where="post",
                color="#a8862c",
                linewidth=0.9,
                linestyle="--",
                alpha=0.85,
                zorder=1,
            )

    # Same colours as ecoDiagramMaker.py -- Random red, policy under test green
    # -- so both thesis figures read with one legend in mind.
    if timeline_a is not None and not timeline_a.empty:
        ax.step(
            timeline_a["elapsed_minutes"],
            timeline_a["mean_rtt_ms"],
            where="post",
            color="#c0392b",
            linewidth=1.8,
            label=f"{phase_a_label} (Phase A)",
            zorder=3,
        )
    if timeline_b is not None and not timeline_b.empty:
        ax.step(
            timeline_b["elapsed_minutes"],
            timeline_b["mean_rtt_ms"],
            where="post",
            color="#1e8449",
            linewidth=1.8,
            label=f"{phase_b_label} (Phase B)",
            zorder=3,
        )

    ax.set_xlabel("Elapsed time since phase start [minutes]")
    ax.set_ylabel("Mean RTT to selected provider [ms]")
    ax.set_xlim(left=0)
    ax.set_ylim(bottom=0)

    fig.suptitle("Latency Indicator by Policy — Direct Comparison", fontsize=13, fontweight="bold", y=0.98)
    ax.set_title(subtitle, fontsize=9.5, color="#555555", pad=10, loc="left")

    # Above the plotting area, right-aligned on the same line as the subtitle:
    # inside the axes it sat on top of the curves it is meant to explain.
    ax.legend(
        loc="lower right",
        bbox_to_anchor=(1.0, 1.0),
        ncol=3,
        frameon=False,
        fontsize=9,
        handlelength=1.6,
        columnspacing=1.2,
        borderaxespad=0.4,
    )
    fig.tight_layout(rect=(0, 0, 1, 0.93))

    fig.savefig(png_out, dpi=300)
    fig.savefig(pdf_out)
    plt.close(fig)


def write_summary(
    path: Path,
    *,
    input_path: Path,
    n_detected: int,
    valid_consumer_ids: list[str],
    missing_consumers: list[str],
    phase_a_label: str,
    phase_b_label: str,
    dur_a: pd.Timedelta | None,
    dur_b: pd.Timedelta | None,
    n_valid: int,
    n_ignored: int,
    n_success_without_rtt: int,
    timeline: pd.DataFrame,
    grid_mode: str,
    grid_minutes: float,
    band: pd.DataFrame | None = None,
    pairs_possible: int = 0,
    outside_band: int = 0,
    borrowed_offsets: list[str] | None = None,
    range_note: str | None = None,
) -> None:
    def fmt_td(td: pd.Timedelta | None) -> str:
        if td is None:
            return "n/a (phase not present in the input)"
        total = td.total_seconds()
        return f"{total / 60:.2f} min ({total:.1f} s)"

    def fmt_ms(v: float) -> str:
        return f"{v:.2f} ms" if pd.notna(v) else "n/a"

    mean_a = timeline.loc[timeline["phase"] == PHASE_A, "mean_rtt_ms"].mean()
    mean_b = timeline.loc[timeline["phase"] == PHASE_B, "mean_rtt_ms"].mean()

    if pd.isna(mean_a) or pd.isna(mean_b):
        diff_line = "- Difference (A -> B): n/a (insufficient data in one phase)\n"
    else:
        abs_diff = mean_b - mean_a
        pct = f"{abs_diff / mean_a * 100.0:+.1f}%" if mean_a != 0 else "n/a"
        # Lower RTT is the improvement a latency-aware policy is meant to bring.
        direction = "lower, i.e. closer providers" if abs_diff < 0 else "higher, i.e. farther providers"
        diff_line = f"- Difference (A -> B): {abs_diff:+.2f} ms ({pct}; Phase B {direction})\n"

    # Where each policy sat inside the observed range: 100% means it matched
    # the fastest RTT anyone measured, 0% means it did as badly as the slowest.
    def captured_pct(curve_mean: float, floor_mean: float, ceiling_mean: float) -> float:
        span = ceiling_mean - floor_mean
        if pd.isna(curve_mean) or pd.isna(span) or span <= 0:
            return float("nan")
        return (ceiling_mean - curve_mean) / span * 100.0

    # Each phase is scored against ITS OWN floor and ceiling, not against the
    # drawn band, which pools both phases.
    def phase_band_means(phase_value: str) -> tuple[float, float]:
        rows = timeline[timeline["phase"] == phase_value]
        if rows.empty or "phase_band_floor" not in rows.columns:
            return float("nan"), float("nan")
        return rows["phase_band_floor"].mean(), rows["phase_band_ceiling"].mean()

    range_lines: list[str] = []
    coverage_warning: str | None = None
    if band is not None and not band.empty:
        mean_floor = band["floor"].mean()
        mean_ceiling = band["ceiling"].mean()
        mean_coverage = band["coverage"].mean()
        x_values = sorted(int(v) for v in band["x_used"].unique() if v > 0)
        x_desc = str(x_values[0]) if len(x_values) == 1 else f"{x_values[0]}–{x_values[-1]}"
        floor_a, ceiling_a = phase_band_means(PHASE_A)
        floor_b, ceiling_b = phase_band_means(PHASE_B)
        pct_a = captured_pct(mean_a, floor_a, ceiling_a)
        pct_b = captured_pct(mean_b, floor_b, ceiling_b)
        range_lines = [
            "\n## Observed RTT range\n\n",
            "At each point in time the range runs from the mean of the x fastest RTTs "
            "measured anywhere in the federation to the mean of the x slowest, with x the "
            "number of Consumers placing then.\n\n",
            f"- RTTs averaged per edge (x): {x_desc}\n",
            f"- Mean floor (fastest measured): {fmt_ms(mean_floor)}\n",
            f"- Mean ceiling (slowest measured): {fmt_ms(mean_ceiling)}\n",
            f"- Share of the range captured by Phase A ({phase_a_label}): "
            + (f"{pct_a:.1f}%\n" if pd.notna(pct_a) else "n/a\n"),
            f"- Share of the range captured by Phase B ({phase_b_label}): "
            + (f"{pct_b:.1f}%\n" if pd.notna(pct_b) else "n/a\n"),
            "- The curve is each Consumer's RTT as measured when it peered, while the range "
            "is built from every probe of the refresh window a point falls in: the two read "
            "the same environment a few tens of seconds apart, so a curve outside the range "
            f"is possible in principle. Points where that happens: {outside_band}.\n",
            f"- Mean coverage: "
            + (f"{mean_coverage * 100:.1f}%" if pd.notna(mean_coverage) else "n/a")
            + f" of the {pairs_possible} Consumer-provider pairs were measured per point\n",
            "\n**Read this range as a floor on what was possible, not the real optimum.** "
            "The simulated delays are not recorded anywhere, so only the pairs the Consumers "
            "actually probed can be ranked: under Random that is the one provider the Broker "
            "masked to, under Latency the three nearest. The fastest pair that nobody probed "
            "is missing from the floor, which therefore sits higher than the true best, and "
            "the policy looks closer to optimal than it is. Exporting the tc delay matrix "
            "from the harness and re-running would remove the caveat.\n",
        ]
        if pd.notna(mean_coverage) and mean_coverage < 0.5:
            coverage_warning = (
                f"- The observed range covers only {mean_coverage * 100:.1f}% of the "
                "Consumer-provider pairs on average: treat its floor as optimistic.\n"
            )

    warnings_lines: list[str] = []
    if borrowed_offsets:
        warnings_lines.append(
            "- "
            + ", ".join(borrowed_offsets)
            + " had too few value changes of its own to locate the refresh boundaries, so it "
            "uses the other phase's estimate. Both phases run the same schedule, which is "
            "what the replay rests on, but a range there is only as good as that assumption.\n"
        )
    if range_note:
        warnings_lines.append(f"- {range_note}\n")
    if coverage_warning:
        warnings_lines.append(coverage_warning)
    min_active = int(timeline["active_consumers"].min()) if not timeline.empty else 0
    max_active = int(timeline["active_consumers"].max()) if not timeline.empty else 0
    if min_active != max_active:
        warnings_lines.append(
            f"- Active Consumer count varied over the run: between {min_active} and "
            f"{max_active} (of {n_detected} detected in the file). The mean is taken over "
            "active Consumers only, so this changes how many readings each point averages, "
            "not its scale.\n"
        )
    if missing_consumers:
        warnings_lines.append(
            f"- {len(missing_consumers)} Consumer(s) never had a successful Peered "
            f"reservation with an RTT reading and contributed no data: {', '.join(missing_consumers)}.\n"
        )
    if n_success_without_rtt:
        warnings_lines.append(
            f"- {n_success_without_rtt} successful Peered row(s) carried no RTT reading "
            "(rtt_ms 0.000 or +Inf) and were left out, so they neither count as a 0ms "
            "connection nor break the forward-filled state.\n"
        )
    if not warnings_lines:
        warnings_lines.append("- None.\n")

    grid_desc = f"regular ({grid_minutes:g} min spacing)" if grid_mode == "regular" else "events (exact timestamps)"

    lines = [
        "# Latency Analysis Summary (direct policy comparison)\n\n",
        "Phase A and Phase B are each measured from their own start and plotted on the "
        "same X axis. The indicator is the mean RTT from each active Consumer to the "
        "provider it is peered with.\n\n",
        f"Input: `{input_path}`\n\n",
        f"- Detected Consumers: {n_detected}\n",
        f"- Consumers with at least one valid reservation: {len(valid_consumer_ids)}\n",
        f"- Phase A ({phase_a_label}) duration: {fmt_td(dur_a)}\n",
        f"- Phase B ({phase_b_label}) duration: {fmt_td(dur_b)}\n",
        f"- Valid successful Peered reservation records with an RTT: {n_valid}\n",
        f"- Ignored records (failed, incomplete, or without an RTT): {n_ignored}\n",
        f"- Timeline grid: {grid_desc}\n",
        "\n## Mean RTT to the selected provider\n\n",
        f"- Phase A ({phase_a_label}): {fmt_ms(mean_a)}\n",
        f"- Phase B ({phase_b_label}): {fmt_ms(mean_b)}\n",
        diff_line,
        *range_lines,
        "\n## Warnings\n\n",
        *warnings_lines,
        "\n## Reproduce\n\n",
        "```bash\n",
        f"python3 federation-tests/scripts/latencyDiagramMaker.py --input {input_path}\n",
        "```\n",
    ]
    path.write_text("".join(lines), encoding="utf-8")


def main(argv: list[str] | None = None) -> None:
    args = parse_args(argv)
    input_path = args.input

    df = load_reservations(input_path)
    if is_eco_csv(df):
        sys.exit(
            "error: no row in this file carries an RTT reading -- it looks like a "
            "comparative-eco reservations.csv. Use federation-tests/scripts/ecoDiagramMaker.py for it."
        )

    valid = filter_valid(df)
    n_ignored = len(df) - len(valid)
    success_peered = (df["outcome"] == "success") & (df["final_phase"] == "Peered")
    n_success_without_rtt = int((success_peered & ~has_rtt_reading(df["rtt_ms"])).sum())
    if valid.empty:
        sys.exit(
            "error: no valid rows found (need outcome=='success', "
            "final_phase=='Peered', and a positive finite rtt_ms)."
        )

    output_dir = args.output_dir or (input_path.parent / "analysis")
    output_dir.mkdir(parents=True, exist_ok=True)

    all_consumer_ids = sorted(df["consumer_id"].dropna().unique())
    valid_consumer_ids = sorted(valid["consumer_id"].unique())
    n_detected = len(all_consumer_ids)
    missing_consumers = sorted(set(all_consumer_ids) - set(valid_consumer_ids))

    phase_a_label = phase_policy_name(df, PHASE_A, "Phase A")
    phase_b_label = phase_policy_name(df, PHASE_B, "Phase B")

    timeline_a = build_phase_local_timeline(valid, PHASE_A, phase_a_label, args.grid, args.grid_minutes)
    timeline_b = build_phase_local_timeline(valid, PHASE_B, phase_b_label, args.grid, args.grid_minutes)
    if timeline_a is None and timeline_b is None:
        sys.exit("error: neither Phase A nor Phase B has any valid data to plot.")

    parts = [t for t in (timeline_a, timeline_b) if t is not None]
    timeline = pd.concat(parts, ignore_index=True)

    # --- Observed RTT range (optional second input) ---
    band = band_a = band_b = None
    pairs_possible = 0
    borrowed_offsets: list[str] = []
    range_note: str | None = None
    if args.no_range:
        range_note = "Observed range left out on request (--no-range)."
    else:
        probes_path = args.probes or (input_path.parent / "probes.csv")
        probes = load_probes(probes_path)
        if probes is None:
            range_note = f"Observed range not shown: {probes_path} is missing or has no RTT readings."
        else:
            pairs_possible = int(
                probes["consumer_id"].nunique() * probes["provider_id"].nunique()
            )
            # Where the refresh boundaries fall, estimated per phase and then
            # shared: under Random a pair is probed once and dropped, so that
            # phase brackets almost nothing and takes the policy phase's
            # estimate, which the replay makes the right one to borrow.
            raw_rows: dict[str, pd.DataFrame] = {}
            estimates: dict[str, tuple[float, float, int]] = {}
            for phase_value, phase_timeline in ((PHASE_A, timeline_a), (PHASE_B, timeline_b)):
                if phase_timeline is None or phase_timeline.empty:
                    continue
                rows = probe_rows(probes, phase_value, phase_timeline["timestamp"].min())
                raw_rows[phase_value] = rows
                estimates[phase_value] = estimate_window_offset(rows, args.range_window_minutes)

            trusted = {
                phase: est[0]
                for phase, est in estimates.items()
                if est[2] >= MIN_OFFSET_BRACKETS and est[1] >= MIN_OFFSET_CONFIDENCE
            }
            borrowed_offsets = sorted(set(estimates) - set(trusted))
            shared = next(iter(trusted.values()), 0.0)
            window_offsets = {phase: trusted.get(phase, shared) for phase in estimates}

            binned_parts = [
                (phase, assign_windows(rows, args.range_window_minutes, window_offsets[phase]))
                for phase, rows in raw_rows.items()
            ]
            # Both phases measure the same replayed environment, so their
            # probes are pooled into one sample per window: that covers more of
            # the Consumer-provider matrix than either phase alone.
            #
            # The curves themselves are left untouched -- each Consumer's RTT
            # as measured when it peered, carried until it peers again.
            # Rebuilding them on the window's measurements would put them
            # inside the range by construction, but it would also move the
            # numbers this thesis already reports, and on these runs they sit
            # inside it anyway (counted below, and written to the summary).
            pooled = [rows for _, rows in binned_parts if not rows.empty]
            pools = pool_by_window(pd.concat(pooled, ignore_index=True)) if pooled else {}
            band_a = observed_range(
                pools, timeline_a, args.range_window_minutes,
                window_offsets.get(PHASE_A, 0.0), pairs_possible,
            )
            band_b = observed_range(
                pools, timeline_b, args.range_window_minutes,
                window_offsets.get(PHASE_B, 0.0), pairs_possible,
            )
            band = merge_bands(band_a, band_b)
            if band is None or band.empty:
                band = None
                range_note = (
                    "Observed range not shown: no probe fell on the phases' timelines."
                )

    # Computed after the rebuild: it is the rebuilt curve that says how many
    # Consumers each point actually counts.
    active_range = timeline["active_consumers"]
    min_active, max_active = int(active_range.min()), int(active_range.max())
    if min_active == max_active:
        subtitle = f"Detected Consumers: {min_active}"
    else:
        subtitle = f"Active Consumers: {min_active}–{max_active} (of {n_detected} detected)"

    png_out = output_dir / "latency_comparison.png"
    pdf_out = output_dir / "latency_comparison.pdf"
    csv_out = output_dir / "latency_comparison.csv"
    tex_out = output_dir / "latency_comparison.tex"
    summary_out = output_dir / "latency_summary.md"

    make_comparison_chart(
        timeline_a,
        timeline_b,
        subtitle,
        phase_a_label,
        phase_b_label,
        png_out,
        pdf_out,
        band=band,
    )

    # The timeline carries the band that was drawn (both phases pooled) with
    # its coverage, plus each phase's own edges, so the figure can be checked
    # and each phase scored against its own range.
    timeline = attach_band_columns(timeline, band, band_a, band_b, args.range_window_minutes)

    # Curve and range are measured a few tens of seconds apart, so this is a
    # check, not a guarantee: the summary reports it either way.
    checkable = timeline.dropna(subset=["phase_band_floor", "phase_band_ceiling"])
    outside_band = int(
        (checkable["mean_rtt_ms"] < checkable["phase_band_floor"] - 1e-6).sum()
        + (checkable["mean_rtt_ms"] > checkable["phase_band_ceiling"] + 1e-6).sum()
    )

    timeline_out = timeline.copy()
    timeline_out["timestamp"] = timeline_out["timestamp"].dt.strftime("%Y-%m-%dT%H:%M:%S.%f%z")
    timeline_out.to_csv(csv_out, index=False)

    # The same figure as LaTeX source, from the same objects the PNG was drawn
    # from, so the two cannot drift apart.
    pgfplotsWriter.write_line_chart(
        tex_out,
        series=[
            (f"{phase_a_label} (Phase A)", "phaseRandom", timeline_a, "mean_rtt_ms"),
            (f"{phase_b_label} (Phase B)", "phasePolicy", timeline_b, "mean_rtt_ms"),
        ],
        band=band,
        band_label="Observed range",
        x_label="Elapsed time since phase start [minutes]",
        y_label="Mean RTT to selected provider [ms]",
        source=Path(__file__).name,
    )

    write_summary(
        summary_out,
        input_path=input_path,
        n_detected=n_detected,
        valid_consumer_ids=valid_consumer_ids,
        missing_consumers=missing_consumers,
        phase_a_label=phase_a_label,
        phase_b_label=phase_b_label,
        dur_a=phase_duration(df, PHASE_A),
        dur_b=phase_duration(df, PHASE_B),
        n_valid=len(valid),
        n_ignored=n_ignored,
        n_success_without_rtt=n_success_without_rtt,
        timeline=timeline,
        grid_mode=args.grid,
        grid_minutes=args.grid_minutes,
        band=band,
        pairs_possible=pairs_possible,
        outside_band=outside_band,
        borrowed_offsets=borrowed_offsets,
        range_note=range_note,
    )

    print("Wrote:")
    for p in (png_out, pdf_out, csv_out, summary_out, tex_out):
        print(f"  {p}")


if __name__ == "__main__":
    main()
