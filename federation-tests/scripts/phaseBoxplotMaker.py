#!/usr/bin/env python3
"""
phaseBoxplotMaker.py
====================

One figure per suite summarising several comparative runs at once: a boxplot
per phase per scale, so a thesis reader sees in one look whether the policy's
advantage survives as the federation grows, and how spread out each phase was.

With the four scales of this thesis that is eight boxes -- 3x7, 8x17, 15x35,
30x70, each with its Random phase next to its policy phase:

    python3 federation-tests/scripts/phaseBoxplotMaker.py \\
        results/comparative-eco/run-3x7 results/comparative-eco/run-8x17 \\
        results/comparative-eco/run-15x35 results/comparative-eco/run-30x70

Each box is the distribution of one phase's timeline points, per Consumer:
carbon intensity per active Consumer for eco, mean RTT for latency. Per
Consumer, because a sum grows with the scale and would squash the small runs
against the axis while the large ones fill the figure.

Nothing here recomputes those series: the pipeline is imported from
ecoDiagramMaker.py and latencyDiagramMaker.py, so a box and the curve of the
same run are built from the same numbers -- including the current-readings
curve for eco and the refresh-window one for latency.

Outputs (default: an `analysis/` directory beside the folder holding the runs;
override with --output-dir):

    <suite>_phase_boxplot.png   -- 300 DPI figure
    <suite>_phase_boxplot.pdf   -- vector version
    <suite>_phase_boxplot.csv   -- n, min, Q1, median, Q3, max and mean per box
    <suite>_phase_boxplot.md    -- per scale, the gap between the two medians

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

# The two chart scripts own the definition of what a phase's series is; this
# one only arranges the result. Importing them keeps a box and the curve of the
# same run from ever disagreeing.
import ecoDiagramMaker as eco  # noqa: E402
import latencyDiagramMaker as lat  # noqa: E402
import pgfplotsWriter  # noqa: E402  (shared LaTeX output; see its docstring)

PHASE_A = eco.PHASE_A
PHASE_B = eco.PHASE_B

RANDOM_COLOUR = "#c0392b"
POLICY_COLOUR = "#1e8449"
# The same khaki the per-run charts shade their range with.
FLOOR_COLOUR = "#a8862c"

# Third series of a scale, beside the two phases: the best the federation
# allowed at each point. It is what makes the distance between a policy and
# the optimum readable, which is the whole reason the boxes sit side by side.
FLOOR_KEY = "floor"


def floor_label(suite: str) -> str:
    """What the third box is called. The two suites differ for a real reason:
    for eco every provider's carbon reading is known, so the box is the minimum
    that was reachable; for latency only the probed pairs are known, so it is
    the minimum that was measured, which can only sit at or above the true
    one."""
    return "Achievable minimum" if suite == "eco" else "Measured minimum"


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="phaseBoxplotMaker.py",
        description=(
            "Boxplot of every phase of several comparative runs in one figure: one box "
            "per phase per scale, values per Consumer so the scales stay comparable. "
            "The suite (eco or latency) is detected from the data."
        ),
        epilog=(
            "example (four scales of the same suite):\n"
            "  python3 federation-tests/scripts/phaseBoxplotMaker.py \\\n"
            "      results/comparative-eco/run-3x7 results/comparative-eco/run-8x17 \\\n"
            "      results/comparative-eco/run-15x35 results/comparative-eco/run-30x70\n"
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "runs",
        nargs="+",
        type=Path,
        help="Run directories (or the reservations.csv inside them), in the order to plot.",
    )
    parser.add_argument(
        "--labels",
        default=None,
        help=(
            "Comma-separated labels for the runs, one per run (default: the topology "
            "read from the data, e.g. 3x7)."
        ),
    )
    parser.add_argument(
        "--output-dir",
        type=Path,
        default=None,
        help="Output directory (default: an 'analysis' folder beside the runs).",
    )
    parser.add_argument(
        "--grid-minutes",
        type=float,
        default=1.0,
        help="Grid spacing in minutes for each run's timeline (default: 1.0).",
    )
    parser.add_argument(
        "--chunks-per-provider",
        type=int,
        default=None,
        help="eco only: override how many Consumers one provider can host (default: read from nodegroups.csv).",
    )
    parser.add_argument(
        "--log-scale",
        action="store_true",
        help=(
            "Logarithmic Y axis, and an _log suffix on every output file. Worth it "
            "for eco, where the policy runs an order of magnitude cleaner than "
            "Random and its boxes are otherwise flattened against the axis."
        ),
    )
    parser.add_argument(
        "--range-window-minutes",
        type=float,
        default=2.0,
        help="latency only: the refresh window the probes are grouped into (default: 2.0).",
    )
    args = parser.parse_args(argv)
    if args.grid_minutes <= 0:
        parser.error("--grid-minutes must be a positive number")
    if args.range_window_minutes <= 0:
        parser.error("--range-window-minutes must be a positive number")
    if args.chunks_per_provider is not None and args.chunks_per_provider < 1:
        parser.error("--chunks-per-provider must be at least 1")
    return args


def reservations_path(run: Path) -> Path:
    """Accepts either a run directory or the CSV itself, so both spellings of
    the same thing work."""
    if run.is_dir():
        return run / "reservations.csv"
    return run


def topology_label(reservations: pd.DataFrame, companion: pd.DataFrame | None) -> str:
    """"3x7" and friends, read from the data rather than from folder names --
    a results directory is called after a timestamp, which says nothing about
    the scale it ran at."""
    consumers = reservations["consumer_id"].nunique()
    providers = 0
    if companion is not None and "provider_id" in companion.columns:
        providers = companion["provider_id"].nunique()
    providers = max(providers, reservations["provider_id"].nunique())
    return f"{consumers}x{providers}"


def eco_series(path: Path, args: argparse.Namespace) -> tuple[dict[str, pd.Series], str, str, str]:
    """One run's two phase series, in carbon intensity per active Consumer."""
    df = eco.load_reservations(path)
    valid = eco.filter_valid(df)
    if valid.empty:
        sys.exit(f"error: {path} has no successful Peered reservation with a carbon reading.")

    labels = {
        PHASE_A: eco.phase_policy_name(df, PHASE_A, "Phase A"),
        PHASE_B: eco.phase_policy_name(df, PHASE_B, "Phase B"),
    }
    timelines = {
        phase: eco.build_phase_local_timeline(valid, phase, labels[phase], "regular", args.grid_minutes)
        for phase in (PHASE_A, PHASE_B)
    }

    ng = eco.load_nodegroups(path.parent / "nodegroups.csv")
    bands = {}
    if ng is not None:
        chunks = args.chunks_per_provider or eco.detect_chunks_per_provider(ng)
        for phase in (PHASE_A, PHASE_B):
            timelines[phase], bands[phase] = eco.rebuild_on_current_readings(
                valid, ng, phase, timelines[phase], chunks
            )

    series = {
        phase: timeline["average_carbon_intensity"].dropna()
        for phase, timeline in timelines.items()
        if timeline is not None and not timeline.empty
    }
    # The best the federation allowed at each point, per Consumer so it lands
    # on the same axis as the two policies. One box per scale, not per phase:
    # both phases replay the same environment, so eco.merge_bands averages
    # their floors exactly as the per-run chart draws them.
    floor = eco.merge_bands(bands.get(PHASE_A), bands.get(PHASE_B))
    if floor is not None and not floor.empty:
        per_consumer = floor["floor"] / floor["x_used"].replace(0, float("nan"))
        series[FLOOR_KEY] = per_consumer.dropna()
    return series, labels[PHASE_A], labels[PHASE_B], topology_label(df, ng)


def latency_series(path: Path, args: argparse.Namespace) -> tuple[dict[str, pd.Series], str, str, str]:
    """One run's two phase series, in mean RTT per active Consumer.

    Nothing to rebuild here, unlike eco: the latency chart plots each
    Consumer's RTT as measured when it peered, so the series is what
    build_phase_local_timeline() already returns."""
    df = lat.load_reservations(path)
    valid = lat.filter_valid(df)
    if valid.empty:
        sys.exit(f"error: {path} has no successful Peered reservation with an RTT reading.")

    labels = {
        PHASE_A: lat.phase_policy_name(df, PHASE_A, "Phase A"),
        PHASE_B: lat.phase_policy_name(df, PHASE_B, "Phase B"),
    }
    timelines = {
        phase: lat.build_phase_local_timeline(valid, phase, labels[phase], "regular", args.grid_minutes)
        for phase in (PHASE_A, PHASE_B)
    }

    series = {
        phase: timeline["mean_rtt_ms"].dropna()
        for phase, timeline in timelines.items()
        if timeline is not None and not timeline.empty
    }

    # The fastest RTTs anyone measured at each point, same treatment as eco:
    # one box per scale, built from both phases' probes pooled per refresh
    # window (see latencyDiagramMaker for why the window boundaries are
    # estimated rather than assumed).
    probes = lat.load_probes(path.parent / "probes.csv")
    if probes is not None:
        raw_rows, estimates = {}, {}
        for phase in (PHASE_A, PHASE_B):
            if timelines[phase] is None or timelines[phase].empty:
                continue
            raw_rows[phase] = lat.probe_rows(probes, phase, timelines[phase]["timestamp"].min())
            estimates[phase] = lat.estimate_window_offset(raw_rows[phase], args.range_window_minutes)
        trusted = {
            phase: est[0]
            for phase, est in estimates.items()
            if est[2] >= lat.MIN_OFFSET_BRACKETS and est[1] >= lat.MIN_OFFSET_CONFIDENCE
        }
        shared = next(iter(trusted.values()), 0.0)
        offsets = {phase: trusted.get(phase, shared) for phase in estimates}
        binned = [lat.assign_windows(rows, args.range_window_minutes, offsets[phase])
                  for phase, rows in raw_rows.items()]
        binned = [rows for rows in binned if not rows.empty]
        if binned:
            pools = lat.pool_by_window(pd.concat(binned, ignore_index=True))
            pairs = int(probes["consumer_id"].nunique() * probes["provider_id"].nunique())
            bands = {
                phase: lat.observed_range(
                    pools, timelines[phase], args.range_window_minutes, offsets.get(phase, 0.0), pairs
                )
                for phase in raw_rows
            }
            floor = lat.merge_bands(bands.get(PHASE_A), bands.get(PHASE_B))
            if floor is not None and not floor.empty:
                series[FLOOR_KEY] = floor["floor"].dropna()
    return series, labels[PHASE_A], labels[PHASE_B], topology_label(df, probes)


def collect(args: argparse.Namespace) -> tuple[str, list[dict]]:
    """Every run's two series, plus which suite they all belong to."""
    suites: set[str] = set()
    collected: list[dict] = []
    for run in args.runs:
        path = reservations_path(run)
        if not path.is_file():
            sys.exit(f"error: no reservations.csv at {path}")
        # An eco and a latency reservations.csv share one header, so the file
        # is told apart by whether any row carries a real RTT.
        raw = pd.read_csv(path, dtype=str)
        if "rtt_ms" not in raw.columns:
            sys.exit(f"error: {path} has no rtt_ms column; is it a comparative reservations.csv?")
        raw["rtt_ms"] = pd.to_numeric(raw["rtt_ms"], errors="coerce")
        suite = "eco" if lat.is_eco_csv(raw) else "latency"
        suites.add(suite)
        if len(suites) > 1:
            sys.exit(
                "error: the runs are not all from the same suite (found eco and latency). "
                "Draw one figure per suite."
            )
        series, label_a, label_b, topology = (eco_series if suite == "eco" else latency_series)(path, args)
        collected.append(
            {"path": path, "series": series, "policy_a": label_a, "policy_b": label_b, "topology": topology}
        )
    return suites.pop(), collected


def apply_labels(collected: list[dict], labels: str | None) -> None:
    if not labels:
        for run in collected:
            run["label"] = run["topology"]
        return
    given = [piece.strip() for piece in labels.split(",")]
    if len(given) != len(collected):
        sys.exit(f"error: --labels has {len(given)} entries but {len(collected)} run(s) were given.")
    for run, label in zip(collected, given):
        run["label"] = label


def make_boxplot(
    collected: list[dict],
    suite: str,
    y_label: str,
    png_out: Path,
    pdf_out: Path,
    log_scale: bool = False,
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
    width = max(7.0, 2.6 * len(collected) + 2.0)
    fig, ax = plt.subplots(figsize=(width, 5.5))

    data: list[pd.Series] = []
    positions: list[float] = []
    colours: list[str] = []
    # A line between one scale and the next, so the three boxes of a group
    # read as a group.
    for boundary in range(len(collected) - 1):
        ax.axvline(boundary + 0.5, color="#bbbbbb", linewidth=0.8, alpha=0.6, zorder=0)

    keys = [(PHASE_A, RANDOM_COLOUR), (PHASE_B, POLICY_COLOUR), (FLOOR_KEY, FLOOR_COLOUR)]
    drawn_floor = any(FLOOR_KEY in run["series"] for run in collected)
    if not drawn_floor:
        keys = keys[:2]
    span = 0.26 if len(keys) == 3 else 0.32
    for index, run in enumerate(collected):
        # The three series of a scale sit next to each other, with a gap to the
        # next scale: the comparisons a reader makes are Random against the
        # policy, and the policy against the best the federation allowed.
        for offset, (key, colour) in enumerate(keys):
            series = run["series"].get(key)
            if series is None or series.empty:
                continue
            data.append(series)
            positions.append(index * 1.0 + (offset - (len(keys) - 1) / 2) * span)
            colours.append(colour)

    # No mean marker: under the policy the distribution is strongly skewed --
    # a handful of points right after a refresh tick, or windows where the whole
    # federation was dirty -- so the mean sits above the box and reads as an
    # error rather than as the skew it is. The median is the line inside the
    # box; the mean is still in the CSV and the summary.
    boxes = ax.boxplot(
        data,
        positions=positions,
        widths=span * 0.85,
        patch_artist=True,
        medianprops={"color": "#222222", "linewidth": 1.4},
        flierprops={"marker": "o", "markersize": 2.5, "markerfacecolor": "#888888", "markeredgecolor": "none"},
    )
    for patch, colour in zip(boxes["boxes"], colours):
        patch.set_facecolor(colour)
        patch.set_alpha(0.35)
        patch.set_edgecolor(colour)
    for element in ("whiskers", "caps"):
        for artist, colour in zip(boxes[element], [c for c in colours for _ in range(2)]):
            artist.set_color(colour)

    ax.set_xticks(range(len(collected)))
    ax.set_xticklabels([run["label"] for run in collected])
    ax.set_xlabel("Consumers x providers")
    ax.set_ylabel(y_label)
    if log_scale:
        # Same axis styling as the eco line chart, so the two figures of a
        # chapter read alike. Nothing to clip here: these are means per
        # Consumer, not sums, so they do not collapse while the federation is
        # still coming up.
        eco.style_log_axis(ax)
    else:
        ax.set_ylim(bottom=0)

    policy_a = collected[0]["policy_a"]
    policy_b = collected[0]["policy_b"]
    handles = [
        plt.Line2D([0], [0], color=RANDOM_COLOUR, linewidth=8, alpha=0.45, label=f"{policy_a} (Phase A)"),
        plt.Line2D([0], [0], color=POLICY_COLOUR, linewidth=8, alpha=0.45, label=f"{policy_b} (Phase B)"),
    ]
    if drawn_floor:
        handles.append(
            plt.Line2D([0], [0], color=FLOOR_COLOUR, linewidth=8, alpha=0.45, label=floor_label(suite))
        )
    title = "Carbon Intensity" if suite == "eco" else "Latency"
    fig.suptitle(f"{title} by Policy and Scale — Distribution per Phase", fontsize=13, fontweight="bold", y=0.98)
    ax.set_title(
        "each box: one phase's timeline points; line = median, box = quartiles, whiskers = 1.5 IQR",
        fontsize=9.5,
        color="#555555",
        pad=10,
        loc="left",
    )
    # Above the plotting area, as in the two line charts, so it never covers a box.
    ax.legend(
        handles=handles, loc="lower right", bbox_to_anchor=(1.0, 1.0),
        ncol=len(handles), frameon=False, fontsize=9,
    )
    fig.tight_layout(rect=(0, 0, 1, 0.93))

    fig.savefig(png_out, dpi=300)
    fig.savefig(pdf_out)
    plt.close(fig)


def box_stats(series: pd.Series) -> dict:
    """The five numbers a boxplot draws, plus the points outside the whiskers.

    Whiskers follow matplotlib's rule -- the most extreme value still within
    1.5 IQR of the quartiles, not the minimum and maximum -- because the LaTeX
    version is generated from these same numbers and has to come out as the
    same figure."""
    q1, median, q3 = (float(series.quantile(q)) for q in (0.25, 0.5, 0.75))
    iqr = q3 - q1
    low_limit, high_limit = q1 - 1.5 * iqr, q3 + 1.5 * iqr
    inside = series[(series >= low_limit) & (series <= high_limit)]
    outliers = sorted(float(v) for v in series[(series < low_limit) | (series > high_limit)])
    return {
        "n": int(series.size),
        "min": float(series.min()),
        "whisker_low": float(inside.min()) if not inside.empty else float(series.min()),
        "q1": q1,
        "median": median,
        "q3": q3,
        "whisker_high": float(inside.max()) if not inside.empty else float(series.max()),
        "max": float(series.max()),
        "mean": float(series.mean()),
        "outliers": outliers,
    }


def summarise(collected: list[dict], suite: str) -> pd.DataFrame:
    rows = []
    for run in collected:
        for phase in (PHASE_A, PHASE_B, FLOOR_KEY):
            series = run["series"].get(phase)
            if series is None or series.empty:
                continue
            policy = {
                PHASE_A: run["policy_a"],
                PHASE_B: run["policy_b"],
                FLOOR_KEY: floor_label(suite),
            }[phase]
            stats = box_stats(series)
            outliers = stats.pop("outliers")
            rows.append(
                {
                    "run": str(run["path"].parent),
                    "label": run["label"],
                    "phase": phase,
                    "policy": policy,
                    **stats,
                    "outliers": " ".join(f"{v:g}" for v in outliers),
                }
            )
    return pd.DataFrame(rows)


# The label escaping and the "how to include this file" header are the same
# for every generated figure, so they live with the rest of the LaTeX output.
tex_escape = pgfplotsWriter.escape


def write_pgfplots(
    path: Path,
    y_label: str,
    table: pd.DataFrame,
    collected: list[dict],
    log_scale: bool,
) -> None:
    """Writes the same figure as pgfplots source, to paste into the thesis.

    Generated from the numbers rather than by converting the matplotlib figure:
    a boxplot is exactly what pgfplots' `boxplot prepared` takes, so the output
    is a handful of readable lines per box -- font, colours and size can be
    retouched by hand -- instead of a machine-made dump nobody can edit."""
    keys = [(PHASE_A, "phaseRandom"), (PHASE_B, "phasePolicy"), (FLOOR_KEY, "phaseFloor")]
    drawn = [(phase, colour) for phase, colour in keys if (table["phase"] == phase).any()]
    per_group = len(drawn)
    group_span = per_group + 1  # one empty slot between scales

    lines = pgfplotsWriter.header(
        path,
        ["statistics"],
        "Boxplot of every phase and scale",
        Path(__file__).name,
        [colour for _, colour in drawn] + ["phaseOutlier"],
    )
    lines += [
        r"\begin{tikzpicture}",
        r"\begin{axis}[",
        "    boxplot/draw direction=y,",
        r"    width=0.95\textwidth, height=7cm,",
        f"    ylabel={{{tex_escape(y_label)}}},",
        r"    xlabel={Consumers $\times$ providers},",
        "    ymode=log, log basis y=10," if log_scale else "    ymin=0,",
    ]

    centres = [i * group_span + (per_group + 1) / 2 for i in range(len(collected))]
    # Midway between the last box of one group and the first of the next.
    boundaries = [(i + 1) * group_span for i in range(len(collected) - 1)]
    lines += [
        "    xtick={" + ", ".join(f"{c:g}" for c in centres) + "},",
        "    xticklabels={" + ", ".join(tex_escape(run["label"]) for run in collected) + "},",
        "    xtick style={draw=none},",
    ]
    if boundaries:
        # The separators between one scale and the next, drawn the way pgfplots
        # draws grid lines rather than as free-hand rules.
        lines += [
            "    extra x ticks={" + ", ".join(f"{b:g}" for b in boundaries) + "},",
            "    extra x tick labels={},",
            "    extra x tick style={grid=major, grid style={gray!45, dashed}, tick style={draw=none}},",
        ]
    lines += [
        "    ymajorgrids=true, grid style={gray!25, dashed},",
        "    legend style={at={(0.5,1.03)}, anchor=south, legend columns=-1, draw=none},",
        "    legend cell align=left,",
        "]",
    ]

    for phase, colour in drawn:
        label = str(table.loc[table["phase"] == phase, "policy"].iloc[0])
        suffix = {PHASE_A: " (Phase A)", PHASE_B: " (Phase B)"}.get(phase, "")
        lines.append(rf"\addlegendimage{{area legend, fill={colour}!35, draw={colour}}}")
        lines.append(rf"\addlegendentry{{{tex_escape(label + suffix)}}}")

    for index, run in enumerate(collected):
        lines.append(f"% --- {run['label']} ---")
        for offset, (phase, colour) in enumerate(drawn):
            rows = table[(table["label"] == run["label"]) & (table["phase"] == phase)]
            if rows.empty:
                continue
            row = rows.iloc[0]
            position = index * group_span + offset + 1
            lines += [
                # `\addplot`, not `\addplot+`: the `+` would add pgfplots' own
                # cycle list on top of these options, and from the sixth plot
                # on that list turns the stroke dashed and changes the outlier
                # mark, so half the boxes of a twelve-box figure would come out
                # in a different style for no reason. Everything the box needs
                # is therefore set here, marks included.
                r"\addplot[boxplot prepared={",
                f"    draw position={position:g},",
                f"    lower whisker={row['whisker_low']:.4g}, lower quartile={row['q1']:.4g},",
                f"    median={row['median']:.4g},",
                f"    upper quartile={row['q3']:.4g}, upper whisker={row['whisker_high']:.4g},",
                f"}}, {colour}, solid, fill={colour}!35,",
                "    mark=*, mark size=1.25pt, "
                "mark options={draw=none, fill=phaseOutlier}]",
            ]
            outliers = str(row["outliers"]).split()
            coordinates = " ".join(f"(0,{value})" for value in outliers)
            lines.append(f"  coordinates {{{coordinates}}};")

    lines += [r"\end{axis}", r"\end{tikzpicture}"]
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def write_summary(path: Path, suite: str, unit: str, table: pd.DataFrame, collected: list[dict]) -> None:
    lines = [
        f"# {'Carbon intensity' if suite == 'eco' else 'Latency'} distribution per phase and scale\n\n",
        "One box per phase per scale; the values are per active Consumer, so the scales can be "
        "read on one axis. The series are the same ones the per-run charts draw.\n\n",
        f"| Scale | Phase | Policy | n | Median [{unit}] | Mean [{unit}] | Q1 | Q3 |\n",
        "|---|---|---|---|---|---|---|---|\n",
    ]
    for _, row in table.iterrows():
        lines.append(
            f"| {row['label']} | {row['phase']} | {row['policy']} | {row['n']} | "
            f"{row['median']:.2f} | {row['mean']:.2f} | {row['q1']:.2f} | {row['q3']:.2f} |\n"
        )

    lines.append("\n## Gap between the two phases, by scale\n\n")
    for run in collected:
        a = run["series"].get(PHASE_A)
        b = run["series"].get(PHASE_B)
        if a is None or b is None or a.empty or b.empty:
            lines.append(f"- {run['label']}: n/a (a phase has no data)\n")
            continue
        median_a, median_b = a.median(), b.median()
        delta = median_b - median_a
        pct = (delta / median_a * 100.0) if median_a else float("nan")
        direction = "lower" if delta < 0 else "higher"
        lines.append(
            f"- {run['label']}: median {median_a:.2f} -> {median_b:.2f} {unit} "
            f"({delta:+.2f}, {pct:+.1f}%, {run['policy_b']} {direction} than {run['policy_a']})\n"
        )
    if any(FLOOR_KEY in run["series"] for run in collected):
        best = "best possible" if suite == "eco" else "best measured"
        lines.append(f"\n## Distance from the {best}, by scale\n\n")
        lines.append(
            "How far each phase's median sat above the median of what the federation offered "
            "at the same moments. A policy on the floor reads 1.00x.\n\n"
        )
        for run in collected:
            floor = run["series"].get(FLOOR_KEY)
            if floor is None or floor.empty:
                continue
            median_floor = floor.median()
            parts = []
            for phase, name in ((PHASE_A, run["policy_a"]), (PHASE_B, run["policy_b"])):
                series = run["series"].get(phase)
                if series is None or series.empty or not median_floor:
                    continue
                parts.append(f"{name} {series.median() / median_floor:.2f}x")
            lines.append(f"- {run['label']}: floor {median_floor:.2f} {unit} — " + ", ".join(parts) + "\n")

    path.write_text("".join(lines), encoding="utf-8")


def default_output_dir(collected: list[dict]) -> Path:
    """Beside the folder that holds the runs, which is where someone comparing
    scales keeps them."""
    parents = {run["path"].parent.parent for run in collected}
    base = parents.pop() if len(parents) == 1 else collected[0]["path"].parent.parent
    return base / "analysis"


def main(argv: list[str] | None = None) -> None:
    args = parse_args(argv)
    suite, collected = collect(args)
    apply_labels(collected, args.labels)

    output_dir = args.output_dir or default_output_dir(collected)
    output_dir.mkdir(parents=True, exist_ok=True)

    unit = "gCO2eq/kWh" if suite == "eco" else "ms"
    y_label = (
        "Carbon intensity per active Consumer [gCO2eq/kWh]"
        if suite == "eco"
        else "Mean RTT to selected provider [ms]"
    )

    # The log version is written beside the linear one, not over it.
    suffix = "_log" if args.log_scale else ""
    png_out = output_dir / f"{suite}_phase_boxplot{suffix}.png"
    pdf_out = output_dir / f"{suite}_phase_boxplot{suffix}.pdf"
    csv_out = output_dir / f"{suite}_phase_boxplot{suffix}.csv"
    md_out = output_dir / f"{suite}_phase_boxplot{suffix}.md"
    tex_out = output_dir / f"{suite}_phase_boxplot{suffix}.tex"

    make_boxplot(collected, suite, y_label, png_out, pdf_out, log_scale=args.log_scale)
    table = summarise(collected, suite)
    table.to_csv(csv_out, index=False)
    write_summary(md_out, suite, unit, table, collected)
    write_pgfplots(tex_out, y_label, table, collected, args.log_scale)

    print("Wrote:")
    for p in (png_out, pdf_out, csv_out, md_out, tex_out):
        print(f"  {p}")


if __name__ == "__main__":
    main()
