# Storage performance measurements

Branch: `codex/storage-performance`. Original source: `7f1a874`.
No API pagination or client changes are included.

## Method

All figures below are medians of six local runs on an Apple M4 Pro,
macOS arm64, Go 1.27.1, with `GOMAXPROCS=4`. Before/after order alternates
between repetitions; benchmark processes run sequentially. Timed workloads run
for 250 ms, except memory batch polling (1,000 drains/run) and SQLite batch
polling (50 drains/run). Fixture setup and refill are outside the measured region.

These are storage microbenchmarks, not production throughput or HTTP latency
claims. Absolute timings depend on the machine, disk, Go version and workload.
The observed ranges are printed by the summary script; small timing changes with
overlapping ranges should not be treated as established gains.

[revisions.json](revisions.json) records independently rebuildable, cumulative
commits: baseline with benchmarks, then changes 1, 2, 3, 4 and 6. The paired
comparisons use the immediately preceding stage to isolate each change.
The index experiment for change 6 is recorded separately from the retained code.

## Retained changes

| Change and measured workload | Before | After | Result |
|---|---:|---:|---:|
| 1. TTL pass, 100 hooks × 1,000 live interactions, nothing expired | 290 µs | 233 µs | 19.7% less time |
| 1. Allocations during that TTL pass | 819,200 B / 100 allocations | 0 B / 0 allocations | Removes temporary pointer arrays |
| 2. Retained bodies after 10,000 inserts, 4 KiB each, limit 1,000 | 39.1 MiB | 3.91 MiB | 90% less retained body data |
| 2. Entire burst plus cleanup/drain | 4.98 ms | 4.71 ms | 5.5% less time |
| 2. Ordinary inserts with a drain every 1,000 entries | 61.3 ns | 67.2 ns | **9.6% more time (~6 ns)** |
| 3. Concurrent SQLite reads (100 × 4 KiB bodies/read), one writer | 600 µs/read | 418 µs/read | 30.4% less time per read |
| 3. Mean writer latency in that concurrent workload | 2.40 ms | 0.125 ms | 94.8% less time waiting/executing |
| 3. SQLite insert without concurrent readers | 54.8 µs | 54.8 µs | No material change |
| 4. Drain 100 memory hooks, one interaction each | 8.55 µs | 7.03 µs | 17.8% lower median; ranges overlap |
| 4. Allocations for that memory batch | 204 | 104 | 49.0% fewer allocations |
| 4. Drain 100 SQLite hooks, one interaction each | 4.43 ms | 1.95 ms | 55.9% less time |
| 6. Memory statistics over 100,000 pending interactions | 125 µs | 19.8 µs | 84.2% less time |
| 6. Ordinary memory insertion | 68.2 ns | 68.4 ns | No established change (overlapping ranges) |
| 6. SQLite metrics / insertion / batch drain | — | — | No material change in retained variant |

Change 1 compacts in place and clears removed references. It still scans the
pending interactions under the memory mutex; lock sharding and bounded cleanup
passes are not implemented. The benchmark isolates allocations and traversal,
not concurrent capture latency during cleanup.

Change 2 bounds **retained interactions**, not allocations or total process RSS.
Every incoming body is still allocated; the burst's total allocated bytes only
decrease by 0.2%. Older interactions are evicted earlier, and `dropped_through`
reports their sequence. The existing eviction metric receives these drops on the
next cleanup pass. Immediate enforcement applies to the memory backend; SQLite
retains its periodic enforcement.

Change 3 adds four read-only SQLite connections under WAL and keeps one writer.
There is no asynchronous capture queue or relaxed acknowledgement of writes.
The mixed benchmark runs four reader workers plus a writer. `writer-ns/op` is a
mean, not p95/p99. Its allocations per read include concurrent writer work; more
writers complete after the change, so that allocation figure is not an isolated
read cost. Reader connections also add some unmeasured fixed memory overhead.

Change 4 routes batches to their backend and uses transactions of at most 64
SQLite hooks. It preserves decode-before-delete: any error rolls back its whole
chunk. Duplicate IDs no longer overwrite a successful drain with an empty
result. Memory polling transfers ownership of its slice rather than copying it;
subsequent captures and eviction cannot mutate the returned slice.

Change 6 maintains exact pending counters in memory and samples runtime memory
statistics outside the storage mutex. SQLite statistics still aggregate rows,
but run on the separate reader pool introduced by change 3. Final measurements
for this change are in `paired/06-metrics-*.txt`.

## Rejected SQLite metrics variants

**Transactional counters maintained by triggers:** the exploratory runs reduced
SQLite metrics from ~2.10 ms to ~3.45 µs, but increased isolated insertion from
~56.4 µs to ~69.9 µs (about 24%). These pilot runs are not alternating comparisons;
they justified testing a lighter alternative, not a precise performance claim.
Raw exploratory results are retained in [pilot](pilot).

**Covering index on interaction type:** the six alternating runs reduced metrics
from 2.23 ms to 0.538 ms (75.9%), but increased isolated insertion from 56.6 µs to
61.8 µs (9.3%) and a 100-hook durable drain from 1.99 ms to 2.54 ms (27.7%).
These write costs apply regardless of scrape frequency. The index was removed
from the final code rather than selecting a tradeoff without a production
read/write profile. Its experiment is reproducible as `06-metrics-index`.

Neither the triggers nor the index are present in the final schema. No cached,
stale metrics or new persistent summary table were introduced.

## Reproduce

From the repository root:

```sh
python3 server/benchmarks/compare.py
python3 server/benchmarks/summarize.py
```

The comparison script archives the recorded local revisions into temporary
folders, compiles their test binaries with the same benchmark source, and
replaces the raw files in [paired](paired). It does not alter the checkout or
contact any external service. Go dependencies must already be available or be
resolvable by the usual Go module tooling. To repeat just one comparison:

```sh
python3 server/benchmarks/compare.py 03-readers
```

For an individual benchmark on the current checkout:

```sh
cd server
go test ./internal/storage -run '^$' -bench '^BenchmarkMemoryCleanup$' -benchmem -benchtime=250ms -count=6 -cpu=4
```

The burst benchmark uses an optional interface assertion to enable the new
insertion limit only on revisions that implement it. This is intentional: the
comparison measures the old periodic policy against immediate enforcement.
The SQLite fixtures use on-disk temporary WAL databases, not `:memory:` databases.

## Validation

Regression tests cover out-of-order timestamps, cleared references, stable cursor
snapshots, stable drained slices, empty poll encoding, sequence continuity,
early eviction accounting, duplicate IDs, batch rollback, reader/writer snapshot
isolation, concurrent capture/read/ack, and counts after each deletion path.

Final checks passed:

```sh
cd server
go test -race ./...
go vet ./...
go test -tags=integration ./test/...
go build ./cmd/hookd
```

The integration suite uses local test listeners. No production system was used
for measurement or validation. Client source files are unchanged.
