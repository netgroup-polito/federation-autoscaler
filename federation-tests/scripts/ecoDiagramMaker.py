#!/usr/bin/env python3
"""
ecoDiagramMaker.py
==================

Direct-comparison carbon-intensity chart for a comparative-eco run. Phase A
(Random) and Phase B (Eco) are each measured from THEIR OWN start (elapsed
minutes since that phase's first valid event), then plotted on the SAME X axis,
so a viewer can compare "N minutes into the phase" directly between the two
policies -- e.g. "at minute 5, was the federation greener under Random or under
Eco?" -- instead of seeing them as two segments of one longer timeline.

Each curve starts at its own X=0, so the dead time between the two phases never
appears and there is nothing to remove or anchor.

Its latency twin is latencyDiagramMaker.py, built the same way so the two thesis
figures read side by side (it plots a mean rather than a sum; see its docstring
for why).

Generic by design: the number of Consumers is auto-detected from the CSV
(nothing is hardcoded), so the exact same command works unchanged for every
experiment size, e.g.:

    python3 federation-tests/scripts/ecoDiagramMaker.py --input results/3c-7p/reservations.csv
    python3 federation-tests/scripts/ecoDiagramMaker.py --input results/8c-17p/reservations.csv
    python3 federation-tests/scripts/ecoDiagramMaker.py --input results/15c-35p/reservations.csv
    python3 federation-tests/scripts/ecoDiagramMaker.py --input results/30c-70p/reservations.csv

Behind the two curves it shades the ACHIEVABLE RANGE: at every point in time,
the sum of the x greenest providers in the federation (the best any policy
could have done) and the sum of the x dirtiest (the worst), where x is the
number of Consumers placing at that moment. A curve sitting on the floor means
the policy took everything there was to take; one in the middle of the band
means it left room. The range comes from `nodegroups.csv`, which records what
every Consumer saw of every provider, so it is measured, not modelled.

Outputs (default: an `analysis/` directory next to the input file; override
with --output-dir):

    carbon_intensity_comparison.png   -- 300+ DPI step chart, both policies overlaid
    carbon_intensity_comparison.pdf   -- vector version of the same chart
    carbon_intensity_comparison.csv   -- the underlying per-phase timeline data
    carbon_summary.md                 -- text summary + sanity-check warnings

`reservations.csv` is the only required input. `nodegroups.csv` (picked up
automatically from the same directory, or passed with --nodegroups) adds the
achievable range; without it the chart is drawn exactly as before.

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
import matplotlib.ticker  # noqa: E402  (log-axis tick locators and formatters)

import pgfplotsWriter  # noqa: E402  (shared LaTeX output; see its docstring)

# Columns this script expects to find in the input CSV. Not all of them feed
# the computation directly (e.g. "action" and "provider_id" are validated for
# completeness / future use), but a comparative-eco reservations.csv always
# carries all of them (see reservationCSVHeader in federation-tests/testlib/writer.go).
REQUIRED_COLUMNS = [
    "timestamp",
    "consumer_id",
    "phase",
    "policy",
    "provider_id",
    "action",
    "carbon_intensity",
    "outcome",
    "final_phase",
]

# The two phase labels this test harness always produces (testlib.PhaseA /
# testlib.PhaseB). Hardcoding these two known literals is what lets phase
# boundaries be "detected automatically from the phase column" without the
# caller having to tell the script which timestamp each phase started at.
PHASE_A = "phase-a"
PHASE_B = "phase-b"

# Columns read from nodegroups.csv, the optional second input that carries what
# every Consumer saw of every provider (see nodegroupCSVHeader in
# federation-tests/testlib/writer.go). This is what makes the achievable range
# measurable: reservations.csv only knows the provider that was chosen.
NODEGROUP_COLUMNS = [
    "timestamp",
    "phase",
    "provider_id",
    "carbon_intensity",
    "has_carbon",
]


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="ecoDiagramMaker.py",
        description=(
            "Overlay Phase A (Random) and Phase B (Eco) on the SAME X axis -- "
            "each measured as elapsed minutes since ITS OWN start -- for a direct, "
            "point-by-point comparison between the two placement policies, instead "
            "of plotting them sequentially on one shared timeline. Works unchanged "
            "for any experiment size -- the number of Consumers is auto-detected "
            "from the CSV."
        ),
        epilog=(
            "examples (identical command, only --input changes with scale):\n"
            "  python3 federation-tests/scripts/ecoDiagramMaker.py --input results/3c-7p/reservations.csv\n"
            "  python3 federation-tests/scripts/ecoDiagramMaker.py --input results/8c-17p/reservations.csv\n"
            "  python3 federation-tests/scripts/ecoDiagramMaker.py --input results/15c-35p/reservations.csv\n"
            "  python3 federation-tests/scripts/ecoDiagramMaker.py --input results/30c-70p/reservations.csv\n"
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "--input",
        required=True,
        type=Path,
        help="Path to a comparative-eco reservations.csv file.",
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
        "--nodegroups",
        type=Path,
        default=None,
        help=(
            "Path to the run's nodegroups.csv, used to shade the achievable "
            "range (default: nodegroups.csv next to --input; the range is "
            "simply left out if that file is not there)."
        ),
    )
    parser.add_argument(
        "--chunks-per-provider",
        type=int,
        default=None,
        help=(
            "How many Consumers one provider can host, i.e. the chunk cap set "
            "in testlib/experiment.go. Read from nodegroups.csv by default; "
            "pass a number to override it. It matters: with 2, the best case "
            "may put two Consumers on the greenest provider, so that provider "
            "counts twice in the floor."
        ),
    )
    parser.add_argument(
        "--no-range",
        action="store_true",
        help="Draw only the two policy curves, even if nodegroups.csv is available.",
    )
    parser.add_argument(
        "--log-scale",
        action="store_true",
        help=(
            "Logarithmic Y axis, and an _log suffix on every output file so both "
            "versions can live side by side. The Eco phase sits an order of "
            "magnitude below Random, so on a linear axis it flattens against the "
            "bottom and its own variation cannot be read. ecoDiagramMakerLog.py is "
            "this same flag under its own name."
        ),
    )
    args = parser.parse_args(argv)
    if args.grid == "regular" and args.grid_minutes <= 0:
        parser.error("--grid-minutes must be a positive number")
    if args.chunks_per_provider is not None and args.chunks_per_provider < 1:
        parser.error("--chunks-per-provider must be at least 1")
    return args


def load_reservations(path: Path) -> pd.DataFrame:
    """Reads and validates the input CSV. Exits with a clear message on any
    problem, per the "produce useful errors" requirement -- this script is
    meant to be run by hand while writing a thesis, not from a pipeline."""
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
            + "\n  this script expects a comparative-eco reservations.csv "
            "(see federation-tests/testlib/writer.go, ReservationRecord) -- a "
            "comparative-latency reservations.csv has no carbon data. Use "
            "federation-tests/scripts/latencyDiagramMaker.py for it."
        )

    # Robust ISO-8601 parsing: Go's time.RFC3339Nano (what writes these
    # timestamps) trims trailing zero fractional digits, so different rows
    # can have different fractional-second precision -- a fixed strptime
    # format would break on that. pd.to_datetime's flexible parser handles
    # the variable precision; errors="coerce" turns anything unparseable
    # into NaT instead of raising, so a handful of bad rows do not kill the
    # whole run.
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

    df["carbon_intensity"] = pd.to_numeric(df["carbon_intensity"], errors="coerce")
    return df


def filter_valid(df: pd.DataFrame) -> pd.DataFrame:
    """Rows that may update a Consumer's active carbon-intensity state:
    a successful, Peered reservation with a numeric carbon_intensity and a
    real consumer_id. Everything else (failed/incomplete records) is
    excluded here and therefore never touches the per-Consumer state --
    which is exactly what "failed reservations must not update a Consumer's
    active carbon-intensity state" requires, simply by construction."""
    consumer_id = df["consumer_id"].fillna("")
    mask = (
        (df["outcome"] == "success")
        & (df["final_phase"] == "Peered")
        & df["carbon_intensity"].notna()
        & df["consumer_id"].notna()
        & (consumer_id.str.strip() != "")
    )
    return df[mask].copy()


def phase_duration(df: pd.DataFrame, phase_value: str) -> pd.Timedelta | None:
    """Wall-clock span of a phase, from the RAW (unfiltered) data -- this is
    the true duration the harness ran that phase for, regardless of whether
    every attempt inside it succeeded."""
    ts = df.loc[df["phase"] == phase_value, "timestamp"]
    if ts.empty:
        return None
    return ts.max() - ts.min()


def phase_policy_name(df: pd.DataFrame, phase_value: str, default: str) -> str:
    """The policy name actually recorded for a phase (e.g. "Random", "Eco"),
    read from the data instead of hardcoded, so the chart/labels stay
    correct even if policy names ever change."""
    values = df.loc[df["phase"] == phase_value, "policy"].dropna()
    values = values[values.str.strip() != ""]
    return values.iloc[0] if not values.empty else default


def build_state_matrix(valid: pd.DataFrame) -> pd.DataFrame:
    """Wide matrix: index = distinct valid-event timestamp (sorted), one
    column per Consumer, values = that Consumer's active carbon intensity
    as of that timestamp. Forward-filled per column, so a cell holds the
    last successful Peered reading until the next one for that Consumer --
    and stays NaN (missing, not zero) before that Consumer's first success,
    per "if no successful reservation exists yet for a Consumer, keep it
    missing"."""
    v = valid.sort_values("timestamp")
    # Guard pivot() against two valid events for the same Consumer landing on
    # the exact same timestamp (should not happen in practice, but pivot()
    # raises on duplicate (index, column) pairs) -- keep the later one.
    v = v.drop_duplicates(subset=["timestamp", "consumer_id"], keep="last")
    wide = v.pivot(index="timestamp", columns="consumer_id", values="carbon_intensity")
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
    """Evaluates the per-Consumer state matrix at target_index. A plain
    .reindex(target_index) would only align on exact timestamp matches and
    leave everything else NaN; unioning the real event index in first, then
    forward-filling, then dropping back to target_index correctly carries
    each Consumer's last known value onto every requested point (regular
    grid or otherwise) while still leaving genuinely-not-yet-active
    Consumers as NaN."""
    unioned = wide.reindex(wide.index.union(target_index)).sort_index().ffill()
    return unioned.reindex(target_index)


def compute_aggregate(state: pd.DataFrame) -> pd.DataFrame:
    active_consumers = state.notna().sum(axis=1)  # int64, kept as a clean count
    aggregate = state.sum(axis=1, skipna=True)
    # active_consumers is 0 exactly when no Consumer has succeeded yet (before
    # anyone's first event). Divide using a float64 copy with those zeros
    # replaced by plain NaN (not pd.NA, which can upcast an int Series to
    # object dtype and behave inconsistently across pandas versions), so
    # average_carbon_intensity is correctly left blank instead of becoming
    # inf or raising a divide-by-zero.
    denom = active_consumers.astype("float64").replace(0.0, float("nan"))
    average = aggregate / denom
    return pd.DataFrame(
        {
            "timestamp": state.index,
            "active_consumers": active_consumers.to_numpy(),
            "aggregate_carbon_intensity": aggregate.to_numpy(),
            "average_carbon_intensity": average.to_numpy(),
        }
    )


def build_phase_local_timeline(
    valid: pd.DataFrame,
    phase_value: str,
    policy_label: str,
    grid: str,
    grid_minutes: float,
) -> pd.DataFrame | None:
    """Builds one phase's own aggregate timeline, measured from THAT PHASE's
    own first valid event (elapsed_minutes == 0 there) instead of a timeline
    shared with the other phase -- this is what makes the two curves overlay
    for a direct comparison rather than sit end to end. Returns None if the
    phase has no valid data at all."""
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
            "aggregate_carbon_intensity",
            "average_carbon_intensity",
        ]
    ]


def load_nodegroups(path: Path) -> pd.DataFrame | None:
    """Reads nodegroups.csv, the per-(Consumer, provider) observation log.

    Returns None -- with a warning, never an error -- when the file is absent
    or unusable: the achievable range is an addition to the chart, and a run
    from before this file existed must still plot."""
    if not path.is_file():
        print(
            f"warning: {path} not found; the chart will show the two policy "
            "curves without the achievable range.",
            file=sys.stderr,
        )
        return None

    try:
        df = pd.read_csv(path, dtype=str)
    except Exception as exc:  # noqa: BLE001 -- surfaced to the user as-is
        print(f"warning: failed to read '{path}' ({exc}); skipping the achievable range.", file=sys.stderr)
        return None

    missing = [c for c in NODEGROUP_COLUMNS if c not in df.columns]
    if missing:
        print(
            f"warning: '{path}' is missing column(s) {', '.join(missing)}; "
            "skipping the achievable range.",
            file=sys.stderr,
        )
        return None

    df["timestamp"] = pd.to_datetime(df["timestamp"], utc=True, errors="coerce")
    df["carbon_intensity"] = pd.to_numeric(df["carbon_intensity"], errors="coerce")
    if "max_size" in df.columns:
        df["max_size"] = pd.to_numeric(df["max_size"], errors="coerce")
    # has_carbon marks a provider that actually advertised a reading; without
    # it a provider that failed its carbon lookup would enter the ranking as
    # a 0 and make the floor artificially green.
    keep = (
        df["timestamp"].notna()
        & df["carbon_intensity"].notna()
        & (df["has_carbon"].astype(str).str.lower() == "true")
        & df["provider_id"].notna()
        & (df["provider_id"].astype(str).str.strip() != "")
    )
    df = df[keep]
    if df.empty:
        print(
            f"warning: '{path}' has no rows with a carbon reading; skipping the achievable range.",
            file=sys.stderr,
        )
        return None
    return df


def detect_chunks_per_provider(ng: pd.DataFrame) -> int:
    """How many Consumers one provider could host during the run.

    Read from `max_size`, the provider's chunk count as the Consumer saw it:
    masking drops it to the reserved count for the providers a policy hid, so
    the largest value across the file is the real cap. It is the single number
    the floor is most sensitive to -- assuming one Consumer per provider when
    a provider could take two makes the floor sum the 30 greenest providers
    instead of the 15 greenest twice, i.e. far too high."""
    if "max_size" not in ng.columns:
        return 1
    largest = pd.to_numeric(ng["max_size"], errors="coerce").max()
    if pd.isna(largest) or largest < 1:
        return 1
    return int(largest)


def build_provider_matrix(ng_phase: pd.DataFrame) -> pd.DataFrame:
    """Wide matrix of what the federation looked like: index = observation
    timestamp, one column per provider, values = that provider's carbon
    intensity as last seen. Several Consumers report the same provider at
    slightly different instants, so observations are collapsed per
    (timestamp, provider) and then forward-filled: at any point in time the
    matrix holds the latest reading known for each provider."""
    collapsed = ng_phase.groupby(["timestamp", "provider_id"])["carbon_intensity"].last()
    return collapsed.unstack().sort_index().ffill()


def build_holdings_matrix(valid: pd.DataFrame) -> pd.DataFrame:
    """Twin of build_state_matrix(), carrying the provider each Consumer is
    peered with instead of the carbon value recorded when it peered. Same
    forward fill, so at any point in time it says who holds what."""
    v = valid.sort_values("timestamp")
    v = v.drop_duplicates(subset=["timestamp", "consumer_id"], keep="last")
    wide = v.pivot(index="timestamp", columns="consumer_id", values="provider_id")
    return wide.sort_index().ffill()


def rebuild_on_current_readings(
    valid: pd.DataFrame,
    ng: pd.DataFrame,
    phase_value: str,
    timeline: pd.DataFrame | None,
    chunks_per_provider: int,
) -> tuple[pd.DataFrame | None, pd.DataFrame | None]:
    """Recomputes one phase's curve, and builds its range, from the SAME
    snapshot of the federation.

    The curve a reservation-only chart can draw carries the carbon intensity
    each Consumer saw when it reserved, and keeps it until that Consumer
    reserves again; the range, built from nodegroups.csv, carries the latest
    reading. Between those two instants the environment moves, and the two
    stop being comparable -- badly enough that the curve could sit below a
    floor that is supposed to bound it.

    So here the curve is rebuilt as "what the providers this Consumer holds
    are worth right now", which is also the more honest indicator: holding a
    provider that has turned dirty is not the same as having picked it while
    it was green. Floor and ceiling then come from the same readings, with x =
    the number of Consumers actually counted, which makes
    floor <= curve <= ceiling true by construction: all three are sums of x
    values drawn from one snapshot.

    Returns (timeline, band); (timeline, None) when there is nothing to
    rebuild from, leaving the caller with the reservation-only curve."""
    if timeline is None or timeline.empty:
        return timeline, None
    ng_phase = ng[ng["phase"] == phase_value]
    phase_valid = valid[valid["phase"] == phase_value]
    if ng_phase.empty or phase_valid.empty:
        return timeline, None

    wide = build_provider_matrix(ng_phase)
    if wide.empty:
        return timeline, None

    index = pd.DatetimeIndex(timeline["timestamp"])
    readings = resample_state(wide, index)
    holdings = resample_state(build_holdings_matrix(phase_valid), index)

    curve: list[float] = []
    floors: list[float] = []
    ceilings: list[float] = []
    providers_seen: list[int] = []
    counted: list[int] = []
    for i in range(len(index)):
        row = readings.iloc[i]
        held = holdings.iloc[i].dropna()
        values = [float(row[p]) for p in held if p in row.index and pd.notna(row[p])]
        available = row.dropna().to_numpy()
        x = len(values)
        providers_seen.append(int(available.size))
        counted.append(x)
        if x == 0 or available.size == 0:
            curve.append(float("nan"))
            floors.append(float("nan"))
            ceilings.append(float("nan"))
            continue
        # The best case must be allowed to pack at least as many Consumers per
        # provider as the run actually did: a reservation whose release went
        # unrecorded can leave three Consumers forward-filled onto a provider
        # that advertised two chunks, and a floor built on two copies could
        # then sit above a curve built on three.
        packed = held.value_counts().max() if not held.empty else 1
        copies = max(1, chunks_per_provider, int(packed))
        slots = sorted(available.tolist() * copies)
        take = min(x, len(slots))
        curve.append(float(sum(values)))
        floors.append(float(sum(slots[:take])))
        ceilings.append(float(sum(slots[-take:])))

    rebuilt = timeline.copy()
    # The reservation-time sum is kept alongside, so a figure drawn before
    # this change can still be reconciled with the new one.
    rebuilt["aggregate_at_choice"] = rebuilt["aggregate_carbon_intensity"]
    rebuilt["aggregate_carbon_intensity"] = curve
    rebuilt["active_consumers"] = counted
    denom = pd.Series(counted, dtype="float64").replace(0.0, float("nan"))
    rebuilt["average_carbon_intensity"] = (pd.Series(curve) / denom).to_numpy()

    band = pd.DataFrame(
        {
            "elapsed_minutes": timeline["elapsed_minutes"].to_numpy(),
            "floor": floors,
            "ceiling": ceilings,
            "providers_seen": providers_seen,
            "x_used": counted,
        }
    )
    return rebuilt, band


def merge_bands(band_a: pd.DataFrame | None, band_b: pd.DataFrame | None) -> pd.DataFrame | None:
    """One band out of the two phases.

    Both phases replay the same environment, so their bands describe the same
    federation and are averaged point by point on elapsed time; where only one
    phase reaches a given minute, that phase's value is used. How far apart the
    two were is reported in the summary, which doubles as a replay check."""
    parts = [b for b in (band_a, band_b) if b is not None and not b.empty]
    if not parts:
        return None
    joined = pd.concat(parts, ignore_index=True)
    # Grid points are generated from each phase's own start, so the same minute
    # can differ in the last decimals between phases; rounding aligns them.
    joined["elapsed_minutes"] = joined["elapsed_minutes"].round(6)
    merged = (
        joined.groupby("elapsed_minutes", as_index=False)
        .agg(
            floor=("floor", "mean"),
            ceiling=("ceiling", "mean"),
            providers_seen=("providers_seen", "max"),
            x_used=("x_used", "max"),
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
) -> pd.DataFrame:
    """Adds the range to the exported timeline: the drawn band (both phases
    averaged) and, next to it, the phase's own floor and ceiling."""
    out = timeline_out.copy()
    key = out["elapsed_minutes"].round(6)

    for column in ("band_floor", "band_ceiling", "phase_band_floor", "phase_band_ceiling"):
        out[column] = float("nan")
    if band is None or band.empty:
        return out

    drawn = band.set_index("elapsed_minutes")
    out["band_floor"] = key.map(drawn["floor"]).to_numpy()
    out["band_ceiling"] = key.map(drawn["ceiling"]).to_numpy()

    for phase_value, phase_band in ((PHASE_A, band_a), (PHASE_B, band_b)):
        if phase_band is None or phase_band.empty:
            continue
        own = phase_band.copy()
        own["elapsed_minutes"] = own["elapsed_minutes"].round(6)
        own = own.drop_duplicates(subset="elapsed_minutes").set_index("elapsed_minutes")
        rows = out["phase"] == phase_value
        out.loc[rows, "phase_band_floor"] = key[rows].map(own["floor"]).to_numpy()
        out.loc[rows, "phase_band_ceiling"] = key[rows].map(own["ceiling"]).to_numpy()
    return out


def band_phase_gap(band_a: pd.DataFrame | None, band_b: pd.DataFrame | None) -> float:
    """Largest distance between the two phases' floors (or ceilings) where both
    reached the same minute. Small means the replay put both phases in front of
    the same federation; large means the band is an average of two different
    environments and the comparison is weaker."""
    if band_a is None or band_b is None or band_a.empty or band_b.empty:
        return float("nan")
    a = band_a.copy()
    b = band_b.copy()
    a["elapsed_minutes"] = a["elapsed_minutes"].round(6)
    b["elapsed_minutes"] = b["elapsed_minutes"].round(6)
    both = a.merge(b, on="elapsed_minutes", suffixes=("_a", "_b"))
    if both.empty:
        return float("nan")
    gap_floor = (both["floor_a"] - both["floor_b"]).abs().max()
    gap_ceiling = (both["ceiling_a"] - both["ceiling_b"]).abs().max()
    return float(max(gap_floor, gap_ceiling))


def apply_log_scale(
    ax: plt.Axes,
    timeline_a: pd.DataFrame | None,
    timeline_b: pd.DataFrame | None,
    band: pd.DataFrame | None,
) -> int:
    """Switches the Y axis to a logarithmic scale and makes it readable.

    Zero is infinitely far away on a log axis, so the usual bottom limit of 0
    cannot be used and the bottom comes from the data. Not from the smallest
    value, though: in the first minute of a phase only one Consumer has peered
    yet, so the sum is a fraction of what it is once the federation is up, and
    anchoring the axis there spends most of the figure on empty space. The
    fifth percentile is used instead, which leaves those ramp-up points below
    the frame -- the count is returned so the summary can say so rather than
    let them disappear quietly.

    The axis itself is styled by style_log_axis(), called first so the limit
    below is set on an axis that is already logarithmic."""
    style_log_axis(ax)

    bottom, below = log_bottom_limit(timeline_a, timeline_b, band)
    if bottom is not None:
        ax.set_ylim(bottom=bottom)
    return below


def log_bottom_limit(
    timeline_a: pd.DataFrame | None,
    timeline_b: pd.DataFrame | None,
    band: pd.DataFrame | None,
) -> tuple[float | None, int]:
    """Where a logarithmic axis should start, and how many points that leaves
    below the frame. Shared with the LaTeX output so both versions of the
    figure cut at the same place."""
    drawn: list[pd.Series] = []
    for frame, column in (
        (timeline_a, "aggregate_carbon_intensity"),
        (timeline_b, "aggregate_carbon_intensity"),
        (band, "floor"),
    ):
        if frame is None or frame.empty or column not in frame.columns:
            continue
        values = frame[column].dropna()
        positive = values[values > 0]
        if not positive.empty:
            drawn.append(positive)
    if not drawn:
        return None, 0

    everything = pd.concat(drawn, ignore_index=True)
    bottom = float(everything.quantile(0.05)) / 1.6
    return bottom, int((everything < bottom).sum())


def style_log_axis(ax: plt.Axes) -> None:
    """Makes a logarithmic Y axis readable: ticks labelled as plain numbers --
    50, 100, 500 read better in a thesis than 10^2 -- and a grid line on the
    minor ticks too, since between one decade and the next there would
    otherwise be nothing to read a value against.

    Shared with phaseBoxplotMaker.py so both figures of a thesis chapter carry
    the same axis."""
    ax.set_yscale("log")
    ax.yaxis.set_major_locator(matplotlib.ticker.LogLocator(base=10.0))
    ax.yaxis.set_major_formatter(matplotlib.ticker.ScalarFormatter())
    ax.yaxis.set_minor_locator(matplotlib.ticker.LogLocator(base=10.0, subs=tuple(range(2, 10))))
    # Decades alone leave long unlabelled stretches -- a value sitting at 42
    # has 10 and 100 to read itself against -- so the halves and fifths of each
    # decade (20, 50, 200, 500) are labelled as well, and the rest are left as
    # grid lines only.
    ax.yaxis.set_minor_formatter(matplotlib.ticker.FuncFormatter(label_round_minor_tick))
    ax.grid(True, which="minor", axis="y", alpha=0.18, linestyle="--", linewidth=0.5)
    ax.tick_params(axis="y", which="minor", labelsize=8.5, colors="#555555")


def label_round_minor_tick(value: float, _position: int) -> str:
    """Labels a minor log tick only when its mantissa is 2 or 5, which keeps
    the axis readable without crowding it."""
    if value <= 0:
        return ""
    decade = 10 ** math.floor(math.log10(value))
    mantissa = round(value / decade)
    if mantissa not in (2, 5):
        return ""
    return f"{value:g}"


def make_comparison_chart(
    timeline_a: pd.DataFrame | None,
    timeline_b: pd.DataFrame | None,
    subtitle: str,
    phase_a_label: str,
    phase_b_label: str,
    png_out: Path,
    pdf_out: Path,
    band: pd.DataFrame | None = None,
    log_scale: bool = False,
) -> int:
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
    # on top of it.
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
            label="Achievable range",
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

    if timeline_a is not None and not timeline_a.empty:
        ax.step(
            timeline_a["elapsed_minutes"],
            timeline_a["aggregate_carbon_intensity"],
            where="post",
            color="#c0392b",
            linewidth=1.8,
            label=f"{phase_a_label} (Phase A)",
            zorder=3,
        )
    if timeline_b is not None and not timeline_b.empty:
        ax.step(
            timeline_b["elapsed_minutes"],
            timeline_b["aggregate_carbon_intensity"],
            where="post",
            color="#1e8449",
            linewidth=1.8,
            label=f"{phase_b_label} (Phase B)",
            zorder=3,
        )

    ax.set_xlabel("Elapsed time since phase start [minutes]")
    ax.set_ylabel("Sum of selected-provider carbon intensities [gCO2eq/kWh]")
    ax.set_xlim(left=0)
    below_axis = 0
    if log_scale:
        below_axis = apply_log_scale(ax, timeline_a, timeline_b, band)
    else:
        ax.set_ylim(bottom=0)

    fig.suptitle("Carbon-Intensity Indicator by Policy — Direct Comparison", fontsize=13, fontweight="bold", y=0.98)
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
    return below_axis


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
    timeline: pd.DataFrame,
    grid_mode: str,
    grid_minutes: float,
    band: pd.DataFrame | None = None,
    band_gap: float = float("nan"),
    chunks_per_provider: int = 1,
    chunks_source: str = "default",
    below_axis: int = 0,
    range_note: str | None = None,
) -> None:
    def fmt_td(td: pd.Timedelta | None) -> str:
        if td is None:
            return "n/a (phase not present in the input)"
        total = td.total_seconds()
        return f"{total / 60:.2f} min ({total:.1f} s)"

    def fmt_val(v: float) -> str:
        return f"{v:.2f} gCO2eq/kWh" if pd.notna(v) else "n/a"

    def diff_block(a: float, b: float, label: str) -> str:
        if pd.isna(a) or pd.isna(b):
            return f"- {label}: n/a (insufficient data in one phase)\n"
        abs_diff = b - a
        pct_diff = (abs_diff / a * 100.0) if a != 0 else float("nan")
        direction = "decrease" if abs_diff < 0 else "increase"
        pct_str = f"{pct_diff:+.1f}%" if pd.notna(pct_diff) else "n/a"
        return f"- {label}: {abs_diff:+.2f} gCO2eq/kWh ({pct_str} {direction} from Phase A to Phase B)\n"

    mean_agg_a = timeline.loc[timeline["phase"] == PHASE_A, "aggregate_carbon_intensity"].mean()
    mean_agg_b = timeline.loc[timeline["phase"] == PHASE_B, "aggregate_carbon_intensity"].mean()
    mean_avg_a = timeline.loc[timeline["phase"] == PHASE_A, "average_carbon_intensity"].mean()
    mean_avg_b = timeline.loc[timeline["phase"] == PHASE_B, "average_carbon_intensity"].mean()

    # Where each policy sat inside the achievable range: 100% means it matched
    # the greenest providers available, 0% means it did as badly as picking the
    # dirtiest ones. Computed on the means, so it answers "over the whole
    # phase", not "at one lucky minute".
    def captured_pct(curve_mean: float, floor_mean: float, ceiling_mean: float) -> float:
        span = ceiling_mean - floor_mean
        if pd.isna(curve_mean) or pd.isna(span) or span <= 0:
            return float("nan")
        return (ceiling_mean - curve_mean) / span * 100.0

    # Each phase is scored against ITS OWN floor and ceiling, not against the
    # drawn band: the drawn one averages the two phases, and scoring a phase
    # against the other phase's environment would put a policy slightly
    # outside its own range.
    def phase_band_means(phase_value: str) -> tuple[float, float]:
        rows = timeline[timeline["phase"] == phase_value]
        if rows.empty or "phase_band_floor" not in rows.columns:
            return float("nan"), float("nan")
        return rows["phase_band_floor"].mean(), rows["phase_band_ceiling"].mean()

    def out_of_band(phase_value: str) -> tuple[int, int]:
        rows = timeline[timeline["phase"] == phase_value]
        if rows.empty or "phase_band_floor" not in rows.columns:
            return 0, 0
        rows = rows.dropna(subset=["phase_band_floor", "phase_band_ceiling"])
        below = int((rows["aggregate_carbon_intensity"] < rows["phase_band_floor"] - 1e-9).sum())
        above = int((rows["aggregate_carbon_intensity"] > rows["phase_band_ceiling"] + 1e-9).sum())
        return below + above, len(rows)

    range_lines: list[str] = []
    mean_floor = band["floor"].mean() if band is not None and not band.empty else float("nan")
    mean_ceiling = band["ceiling"].mean() if band is not None and not band.empty else float("nan")
    if band is not None and not band.empty:
        x_values = sorted(int(v) for v in band["x_used"].unique() if v > 0)
        x_desc = str(x_values[0]) if len(x_values) == 1 else f"{x_values[0]}–{x_values[-1]}"
        floor_a, ceiling_a = phase_band_means(PHASE_A)
        floor_b, ceiling_b = phase_band_means(PHASE_B)
        pct_a = captured_pct(mean_agg_a, floor_a, ceiling_a)
        pct_b = captured_pct(mean_agg_b, floor_b, ceiling_b)
        outside_a, points_a = out_of_band(PHASE_A)
        outside_b, points_b = out_of_band(PHASE_B)
        if outside_a or outside_b:
            warn_outside = (
                f"- {outside_a + outside_b} of {points_a + points_b} plotted point(s) sat "
                "just outside their own range. A reservation carries the carbon value its "
                "Consumer saw when it reserved, which can lag the federation's latest "
                "reading by the observation delay (provider cache plus advertisement "
                "cycle), so around a refresh tick the curve and the range can be one tick "
                "apart.\n"
            )
        else:
            warn_outside = None
        range_lines = [
            "\n## Achievable range\n\n",
            "At each point in time the range runs from the sum of the x greenest providers "
            "in the federation to the sum of the x dirtiest, with x the number of Consumers "
            "placing then. It is what the federation made possible, measured from "
            "nodegroups.csv.\n\n",
            f"- Providers summed per point (x): {x_desc}\n",
            f"- Consumers one provider can host: {chunks_per_provider} ({chunks_source})\n",
            "- The curve is the carbon intensity those providers carry **at that moment**, "
            "read from the same snapshot as the range, not the value recorded when each "
            "Consumer reserved (kept in the CSV as `aggregate_at_choice`). Holding a "
            "provider that has turned dirty counts as dirty.\n",
            f"- Mean floor (best possible): {fmt_val(mean_floor)}\n",
            f"- Mean ceiling (worst possible): {fmt_val(mean_ceiling)}\n",
            f"- Share of the range captured by Phase A ({phase_a_label}): "
            + (f"{pct_a:.1f}%\n" if pd.notna(pct_a) else "n/a\n"),
            f"- Share of the range captured by Phase B ({phase_b_label}): "
            + (f"{pct_b:.1f}%\n" if pd.notna(pct_b) else "n/a\n"),
            "- 100% means the policy matched the greenest providers available; 0% means it "
            "did as badly as the dirtiest. Each phase is scored against its own range, "
            "while the shaded band averages the two.\n",
        ]
        if pd.notna(band_gap):
            range_lines.append(
                f"- Largest gap between the two phases' own ranges: {band_gap:.2f} gCO2eq/kWh "
                "(small means both phases really did face the same federation).\n"
            )

    warnings_lines: list[str] = []
    if below_axis:
        warnings_lines.append(
            f"- Logarithmic axis: {below_axis} value(s) fall below the bottom of the frame. "
            "They are the first minute of a phase, when only part of the Consumers have "
            "peered and the sum is a fraction of its steady-state value; anchoring the axis "
            "there would spend most of the figure on empty space. They are all in the CSV.\n"
        )
    if range_note:
        warnings_lines.append(f"- {range_note}\n")
    if band is not None and not band.empty and warn_outside:
        warnings_lines.append(warn_outside)
    if band is not None and not band.empty:
        short = band[band["x_used"] < band["x_used"].max()]
        if not short.empty and int(band["providers_seen"].min()) * chunks_per_provider < int(
            timeline["active_consumers"].max()
        ):
            warnings_lines.append(
                "- At some points the federation had fewer provider slots than Consumers "
                "placing, so the range there covers every slot there was rather than x of them.\n"
            )
    min_active = int(timeline["active_consumers"].min()) if not timeline.empty else 0
    max_active = int(timeline["active_consumers"].max()) if not timeline.empty else 0
    if min_active != max_active:
        warnings_lines.append(
            f"- Active Consumer count varied over the run: between {min_active} and "
            f"{max_active} (of {n_detected} detected in the file).\n"
        )
    if missing_consumers:
        warnings_lines.append(
            f"- {len(missing_consumers)} Consumer(s) never had a successful Peered "
            f"reservation and contributed no data: {', '.join(missing_consumers)}.\n"
        )
    if not warnings_lines:
        warnings_lines.append("- None.\n")

    grid_desc = f"regular ({grid_minutes:g} min spacing)" if grid_mode == "regular" else "events (exact timestamps)"

    lines = [
        "# Carbon Intensity Analysis Summary (direct policy comparison)\n\n",
        "Phase A and Phase B are each measured from their own start and plotted on "
        "the same X axis.\n\n",
        f"Input: `{input_path}`\n\n",
        f"- Detected Consumers: {n_detected}\n",
        f"- Consumers with at least one valid reservation: {len(valid_consumer_ids)}\n",
        f"- Phase A ({phase_a_label}) duration: {fmt_td(dur_a)}\n",
        f"- Phase B ({phase_b_label}) duration: {fmt_td(dur_b)}\n",
        f"- Valid successful Peered reservation records: {n_valid}\n",
        f"- Ignored failed/incomplete records: {n_ignored}\n",
        f"- Timeline grid: {grid_desc}\n",
        "\n## Aggregate carbon-intensity indicator\n\n",
        f"- Mean aggregate (Phase A, {phase_a_label}): {fmt_val(mean_agg_a)}\n",
        f"- Mean aggregate (Phase B, {phase_b_label}): {fmt_val(mean_agg_b)}\n",
        diff_block(mean_agg_a, mean_agg_b, "Aggregate difference (A -> B)"),
        "\n## Mean per-active-Consumer carbon intensity\n\n",
        f"- Phase A ({phase_a_label}): {fmt_val(mean_avg_a)}\n",
        f"- Phase B ({phase_b_label}): {fmt_val(mean_avg_b)}\n",
        diff_block(mean_avg_a, mean_avg_b, "Per-Consumer difference (A -> B)"),
        *range_lines,
        "\n## Warnings\n\n",
        *warnings_lines,
        "\n## Reproduce\n\n",
        "```bash\n",
        f"python3 federation-tests/scripts/ecoDiagramMaker.py --input {input_path}\n",
        "```\n",
    ]
    path.write_text("".join(lines), encoding="utf-8")


def main(argv: list[str] | None = None) -> None:
    args = parse_args(argv)
    input_path = args.input
    output_dir = args.output_dir or (input_path.parent / "analysis")
    output_dir.mkdir(parents=True, exist_ok=True)

    df = load_reservations(input_path)
    valid = filter_valid(df)
    n_ignored = len(df) - len(valid)
    if valid.empty:
        sys.exit(
            "error: no valid rows found (need outcome=='success', "
            "final_phase=='Peered', and a numeric carbon_intensity).\n"
            "  The two suites share these columns but not the data, so a "
            "comparative-latency reservations.csv gets this far and then has no "
            "carbon to plot. Use federation-tests/scripts/latencyDiagramMaker.py "
            "for it."
        )

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

    # --- Achievable range, and the curve rebuilt on the same readings ---
    band = band_a = band_b = None
    range_note: str | None = None
    chunks_per_provider = args.chunks_per_provider or 1
    chunks_source = "given with --chunks-per-provider"
    if args.no_range:
        range_note = (
            "Achievable range left out on request (--no-range); the curve is the carbon "
            "intensity recorded when each Consumer reserved."
        )
    else:
        ng_path = args.nodegroups or (input_path.parent / "nodegroups.csv")
        ng = load_nodegroups(ng_path)
        if ng is None:
            range_note = (
                f"Achievable range not shown: {ng_path} is missing or carries no carbon "
                "readings. The curve is the carbon intensity recorded when each Consumer "
                "reserved, which cannot be checked against the federation's own readings."
            )
        else:
            if args.chunks_per_provider is None:
                chunks_per_provider = detect_chunks_per_provider(ng)
                chunks_source = "read from nodegroups.csv (max_size)"
            timeline_a, band_a = rebuild_on_current_readings(
                valid, ng, PHASE_A, timeline_a, chunks_per_provider
            )
            timeline_b, band_b = rebuild_on_current_readings(
                valid, ng, PHASE_B, timeline_b, chunks_per_provider
            )
            parts = [t for t in (timeline_a, timeline_b) if t is not None]
            timeline = pd.concat(parts, ignore_index=True)
            band = merge_bands(band_a, band_b)
            if band is None or band.empty:
                band = None
                range_note = (
                    "Achievable range not shown: nodegroups.csv had no provider readings "
                    "on the phases' timelines."
                )

    # Computed after the rebuild: it is the rebuilt curve that says how many
    # Consumers each point actually counts.
    active_range = timeline["active_consumers"]
    min_active, max_active = int(active_range.min()), int(active_range.max())
    if min_active == max_active:
        subtitle = f"Detected Consumers: {min_active}"
    else:
        subtitle = f"Active Consumers: {min_active}–{max_active} (of {n_detected} detected)"

    # The log-scale version writes beside the linear one instead of over it:
    # the two answer different questions and a thesis may well use both.
    suffix = "_log" if args.log_scale else ""
    png_out = output_dir / f"carbon_intensity_comparison{suffix}.png"
    pdf_out = output_dir / f"carbon_intensity_comparison{suffix}.pdf"
    csv_out = output_dir / f"carbon_intensity_comparison{suffix}.csv"
    summary_out = output_dir / f"carbon_summary{suffix}.md"
    tex_out = output_dir / f"carbon_intensity_comparison{suffix}.tex"

    below_axis = make_comparison_chart(
        timeline_a,
        timeline_b,
        subtitle,
        phase_a_label,
        phase_b_label,
        png_out,
        pdf_out,
        band=band,
        log_scale=args.log_scale,
    )

    # The timeline carries both the band that was drawn (the two phases
    # averaged) and each phase's own, so the numbers behind the figure can be
    # checked and each phase can be scored against its own range.
    timeline = attach_band_columns(timeline, band, band_a, band_b)

    timeline_out = timeline.copy()
    timeline_out["timestamp"] = timeline_out["timestamp"].dt.strftime("%Y-%m-%dT%H:%M:%S.%f%z")
    timeline_out.to_csv(csv_out, index=False)

    # The same figure as LaTeX source, from the same objects the PNG was drawn
    # from, so the two cannot drift apart.
    log_bottom = log_bottom_limit(timeline_a, timeline_b, band)[0] if args.log_scale else None
    pgfplotsWriter.write_line_chart(
        tex_out,
        series=[
            (f"{phase_a_label} (Phase A)", "phaseRandom", timeline_a, "aggregate_carbon_intensity"),
            (f"{phase_b_label} (Phase B)", "phasePolicy", timeline_b, "aggregate_carbon_intensity"),
        ],
        band=band,
        band_label="Achievable range",
        x_label="Elapsed time since phase start [minutes]",
        y_label="Sum of selected-provider carbon intensities [gCO2eq/kWh]",
        log_scale=args.log_scale,
        bottom=log_bottom,
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
        timeline=timeline,
        grid_mode=args.grid,
        grid_minutes=args.grid_minutes,
        band=band,
        band_gap=band_phase_gap(band_a, band_b),
        chunks_per_provider=chunks_per_provider,
        chunks_source=chunks_source,
        below_axis=below_axis,
        range_note=range_note,
    )

    print("Wrote:")
    for p in (png_out, pdf_out, csv_out, summary_out, tex_out):
        print(f"  {p}")


if __name__ == "__main__":
    main()
