#!/usr/bin/env python3
"""Print medians and observed ranges from compare.py's raw Go output."""
import pathlib
import statistics

HERE = pathlib.Path(__file__).resolve().parent


def read(path):
    results = {}
    for line in path.read_text().splitlines():
        if not line.startswith("Benchmark"):
            continue
        fields = line.split()
        metrics = results.setdefault(fields[0], {})
        for index in range(2, len(fields) - 1, 2):
            metrics.setdefault(fields[index + 1], []).append(float(fields[index]))
    return results


if __name__ == "__main__":
    for before_path in sorted((HERE / "paired").glob("*-before.txt")):
        after_path = before_path.with_name(before_path.name.replace("-before", "-after"))
        before, after = read(before_path), read(after_path)
        print(before_path.name.removesuffix("-before.txt"))
        for benchmark, metrics in before.items():
            for unit, old in metrics.items():
                new = after[benchmark][unit]
                old_median, new_median = statistics.median(old), statistics.median(new)
                change = f"{100 * (new_median / old_median - 1):+.1f}%" if old_median else "n/a"
                print(f"  {benchmark} {unit}: {old_median:g} -> {new_median:g} ({change}); "
                      f"ranges [{min(old):g}, {max(old):g}] -> [{min(new):g}, {max(new):g}]; n={len(old)}/{len(new)}")
