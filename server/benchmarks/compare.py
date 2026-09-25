#!/usr/bin/env python3
"""Build recorded local revisions and run alternating before/after benchmarks.

Run from any directory: python3 server/benchmarks/compare.py
No production service or external network target is contacted.
"""
import json
import pathlib
import subprocess
import tempfile
import sys
import platform

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parent.parent
REVISIONS = json.loads((HERE / "revisions.json").read_text())
# Each comparison changes one feature. Batches use a fixed number of drains;
# setup/refill is excluded from Go's measured time, but still takes wall time.
CASES = [
    ("01-cleanup", "00-baseline", "01-cleanup", "BenchmarkMemoryCleanup$", "250ms"),
    ("02-limit", "01-cleanup", "02-limit", "BenchmarkMemory(Burst|Add)$", "250ms"),
    ("03-readers", "02-limit", "03-readers", "BenchmarkSQLite(Mixed|Add)$", "250ms"),
    ("04-batch-memory", "03-readers", "04-batch", "BenchmarkCompositePollBatch/durable=false$", "1000x"),
    ("04-batch-sqlite", "03-readers", "04-batch", "BenchmarkCompositePollBatch/durable=true$", "50x"),
    ("06-index", "04-batch", "06-metrics-index", "Benchmark(MemoryStats|MemoryAdd|SQLiteStats|SQLiteAdd)$", "250ms"),
    ("06-index-batch", "04-batch", "06-metrics-index", "BenchmarkCompositePollBatch/durable=true$", "50x"),
    ("06-metrics", "04-batch", "06-metrics", "Benchmark(MemoryStats|MemoryAdd|SQLiteStats|SQLiteAdd)$", "250ms"),
    ("06-metrics-batch", "04-batch", "06-metrics", "BenchmarkCompositePollBatch/durable=true$", "50x"),
]


def run():
    selected = [case for case in CASES if not sys.argv[1:] or case[0] in sys.argv[1:]]
    if not selected:
        raise SystemExit("No matching comparison")
    stages = {stage for case in selected for stage in case[1:3]}
    output = HERE / "paired"
    output.mkdir(exist_ok=True)
    environment = {
        "go": subprocess.check_output(["go", "version"], text=True).strip(),
        "platform": " ".join((platform.system(), platform.release(), platform.machine())),
        "repetitions": 6,
        "GOMAXPROCS": 4,
    }
    (output / "environment.json").write_text(json.dumps(environment, indent=2) + "\n")
    with tempfile.TemporaryDirectory(prefix="hookd-bench-") as temporary:
        temp = pathlib.Path(temporary)
        binaries = {}
        for stage, revision in REVISIONS.items():
            if stage not in stages:
                continue
            print(f"Building {stage}: {revision}", flush=True)
            checkout = temp / stage
            checkout.mkdir()
            archive = temp / f"{stage}.tar"
            subprocess.run(["git", "archive", "--format=tar", f"--output={archive}", revision, "server"], cwd=ROOT, check=True)
            subprocess.run(["tar", "-xf", str(archive), "-C", str(checkout)], check=True)
            binary = temp / f"{stage}.test"
            subprocess.run(["go", "test", "-c", "-o", str(binary), "./internal/storage"], cwd=checkout / "server", check=True)
            binaries[stage] = binary
        for name, before, after, pattern, duration in selected:
            print(f"Measuring {name}", flush=True)
            with (output / f"{name}-before.txt").open("w") as old, (output / f"{name}-after.txt").open("w") as new:
                variants = [(before, old), (after, new)]
                for repetition in range(6):
                    for stage, stream in variants[::1 if repetition % 2 == 0 else -1]:
                        subprocess.run([
                            str(binaries[stage]), "-test.run=^$", f"-test.bench={pattern}",
                            f"-test.benchtime={duration}", "-test.count=1", "-test.cpu=4",
                        ], cwd=ROOT / "server", stdout=stream, stderr=subprocess.STDOUT, check=True)
                        stream.flush()
    print(f"Results: {output}", flush=True)


if __name__ == "__main__":
    run()
