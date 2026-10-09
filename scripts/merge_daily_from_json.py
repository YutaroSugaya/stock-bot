#!/usr/bin/env python3
"""Merge freshly-fetched daily bars into the existing harness CSVs.

Refresh flow (see docs/runtime/EDGE_TESTING_DATA.md §A2):
  1. Obtain Yahoo-chart-style daily bars under the provider's terms and save them
     as JSON in the shape below (this repo does not ship a fetcher).
  2. python3 scripts/merge_daily_from_json.py <fetched.json> [data_dir]

The JSON is {"data": {"<sym>": [["YYYY-MM-DD", open, high, low, close, vol], ...]}}.
For each symbol that ALREADY has <data_dir>/<sym>_daily.csv, fetched bars are merged
BY DATE (fetched wins on overlap — this corrects a previously-partial last bar) and
the file is rewritten sorted ascending. New symbols are skipped (whitelist by file).
Full history is preserved (only overlapping/newer dates change). READ-ONLY w.r.t. the
broker; touches only local CSVs.
"""
import json
import os
import sys

HEADER = "DateJST;Open;High;Low;Close;Volume"


def num(x):
    f = float(x)
    return str(int(f)) if f == int(f) else str(f)


def main():
    if len(sys.argv) < 2:
        print("usage: merge_daily_from_json.py <fetched.json> [data_dir]")
        sys.exit(2)
    payload = json.load(open(sys.argv[1]))
    data = payload.get("data", payload)  # accept {"data":{...}} or {...}
    ddir = sys.argv[2] if len(sys.argv) > 2 else "backend/data"

    refreshed = skipped = 0
    for sym, rows in data.items():
        path = os.path.join(ddir, f"{sym}_daily.csv")
        if not rows or not os.path.exists(path):
            skipped += 1
            continue
        by_date = {}
        with open(path) as f:
            f.readline()  # header
            for line in f:
                line = line.strip()
                if line:
                    by_date[line.split(";")[0]] = line
        before_last = max(by_date) if by_date else "-"
        for r in rows:  # fetched overrides existing on the same date (fixes partial bars)
            d, o, h, l, c, v = r
            by_date[d] = ";".join([d, num(o), num(h), num(l), num(c), num(v)])
        with open(path, "w") as f:
            f.write(HEADER + "\n")
            f.write("\n".join(by_date[d] for d in sorted(by_date)) + "\n")
        refreshed += 1
    print(f"refreshed {refreshed} symbols, skipped {skipped} (no existing CSV / empty)")
    # report the new max date across refreshed files
    dates = []
    for sym in data:
        p = os.path.join(ddir, f"{sym}_daily.csv")
        if os.path.exists(p):
            with open(p) as f:
                last = f.readlines()[-1].split(";")[0]
                dates.append(last)
    if dates:
        print("latest bar date now:", max(dates))


if __name__ == "__main__":
    main()
