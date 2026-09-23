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

Outputs (default: an `analysis/` directory next to the input file; override
with --output-dir):

    carbon_intensity_comparison.png   -- 300+ DPI step chart, both policies overlaid
    carbon_intensity_comparison.pdf   -- vector version of the same chart
    carbon_intensity_comparison.csv   -- the underlying per-phase timeline data
    carbon_summary.md                 -- text summary + sanity-check warnings

Only `reservations.csv` is read; no other experiment output file is required.

Requires: Python 3, pandas, matplotlib (standard library otherwise).
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

import pandas as pd

import matplotlib

matplotlib.use("Agg")  # headless-safe: no display needed (e.g. on a remote server)
import matplotlib.pyplot as plt  # noqa: E402  (must follow matplotlib.use)

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
    args = parser.parse_args(argv)
    if args.grid == "regular" and args.grid_minutes <= 0:
        parser.error("--grid-minutes must be a positive number")
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
            "comparative-latency reservations.csv has no carbon data."
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


def make_comparison_chart(
    timeline_a: pd.DataFrame | None,
    timeline_b: pd.DataFrame | None,
    subtitle: str,
    phase_a_label: str,
    phase_b_label: str,
    png_out: Path,
    pdf_out: Path,
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
    fig, ax = plt.subplots(figsize=(10, 5.5))

    if timeline_a is not None and not timeline_a.empty:
        ax.step(
            timeline_a["elapsed_minutes"],
            timeline_a["aggregate_carbon_intensity"],
            where="post",
            color="#c0392b",
            linewidth=1.8,
            label=f"{phase_a_label} (Phase A)",
        )
    if timeline_b is not None and not timeline_b.empty:
        ax.step(
            timeline_b["elapsed_minutes"],
            timeline_b["aggregate_carbon_intensity"],
            where="post",
            color="#1e8449",
            linewidth=1.8,
            label=f"{phase_b_label} (Phase B)",
        )

    ax.set_xlabel("Elapsed time since phase start [minutes]")
    ax.set_ylabel("Sum of selected-provider carbon intensities [gCO2eq/kWh]")
    ax.set_xlim(left=0)
    ax.set_ylim(bottom=0)

    fig.suptitle("Carbon-Intensity Indicator by Policy — Direct Comparison", fontsize=13, fontweight="bold", y=0.98)
    ax.set_title(subtitle, fontsize=9.5, color="#555555", pad=10)

    ax.legend(loc="best", frameon=True, framealpha=0.9)
    fig.tight_layout(rect=(0, 0, 1, 0.94))

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
    timeline: pd.DataFrame,
    grid_mode: str,
    grid_minutes: float,
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

    warnings_lines: list[str] = []
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
            "final_phase=='Peered', and a numeric carbon_intensity). "
            "Is this a comparative-eco reservations.csv?"
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

    active_range = timeline["active_consumers"]
    min_active, max_active = int(active_range.min()), int(active_range.max())
    if min_active == max_active:
        subtitle = f"Detected Consumers: {min_active}"
    else:
        subtitle = f"Active Consumers: {min_active}–{max_active} (of {n_detected} detected)"

    png_out = output_dir / "carbon_intensity_comparison.png"
    pdf_out = output_dir / "carbon_intensity_comparison.pdf"
    csv_out = output_dir / "carbon_intensity_comparison.csv"
    summary_out = output_dir / "carbon_summary.md"

    make_comparison_chart(timeline_a, timeline_b, subtitle, phase_a_label, phase_b_label, png_out, pdf_out)

    timeline_out = timeline.copy()
    timeline_out["timestamp"] = timeline_out["timestamp"].dt.strftime("%Y-%m-%dT%H:%M:%S.%f%z")
    timeline_out.to_csv(csv_out, index=False)

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
    )

    print("Wrote:")
    for p in (png_out, pdf_out, csv_out, summary_out):
        print(f"  {p}")


if __name__ == "__main__":
    main()
