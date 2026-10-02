#!/usr/bin/env python3
"""
ecoDiagramMakerLog.py
=====================

ecoDiagramMaker.py with a logarithmic Y axis. Same inputs, same computation,
same outputs -- only the axis changes, and the file names take an `_log`
suffix so this version sits beside the linear one instead of replacing it.

Why it exists: under Eco the federation runs an order of magnitude cleaner
than under Random (a median around 42 gCO2eq/kWh per Consumer against 420), so
on a linear axis Phase B is a flat line squashed against the bottom and its own
variation -- which is the interesting part, how close it tracks the greenest
providers available -- cannot be read at all. A log axis gives both phases room.

    python3 federation-tests/scripts/ecoDiagramMakerLog.py \\
        --input results/comparative-eco/<timestamp>/reservations.csv

Every option of ecoDiagramMaker.py works here unchanged (--output-dir,
--nodegroups, --chunks-per-provider, --grid, --grid-minutes, --no-range); this
is the same program with --log-scale always on, so there is one implementation
of the curve and the range, not two that can drift apart.

Requires: Python 3, pandas, matplotlib (standard library otherwise).
"""

from __future__ import annotations

import sys

import ecoDiagramMaker as eco


def main(argv: list[str] | None = None) -> None:
    args = list(sys.argv[1:] if argv is None else argv)
    if "--log-scale" not in args:
        args.append("--log-scale")
    eco.main(args)


if __name__ == "__main__":
    main()
