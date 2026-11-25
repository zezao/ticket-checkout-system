# Outbox Relay Tuning — 2026-11-24

Follow-up to the [baseline](2026-11-24-baseline.md). Application commit
`5e23710` (Phase 5), plus the worker tuning described here (working tree at run
time). The API was not restarted during these runs.

## Finding

The baseline relay claimed 100 rows every second, a hard ceiling of 100 rows/s.
A uniform hold/release cycle writes two `seat.changed` rows, so at ~2.4k
successful cycles/s the outbox produced ~4.8k rows/s. The relay fell behind and
the outbox lag reached ~26 s in 30 s.

## Change

The relay is now backlog-aware:

- Batch size: 100 → 1,000 rows.
- Idle interval: 1 s (the baseline cadence).
- When a full batch is claimed, the next tick runs immediately (1 ms timer)
  until the backlog drops below one batch, then the relay returns to 1 s.

A first attempt used a fixed 100 ms interval with 1,000-row batches. It fixed
the lag but reduced hot-seat throughput by ~9% and raised its p99 by ~3 ms,
because it kept querying and marking even when the queue was nearly empty. The
adaptive idle/busy version avoids that.

## Results

Uniform, 50 VUs, 30 s:

| Metric | Baseline | Adaptive |
| --- | --- | --- |
| Iterations | 74,102 (2,468.2/s) | 74,604 (2,484.2/s) |
| Successful cycles | 72,118 (2,402.1/s) | 72,565 (2,416.3/s) |
| Conflicts | 1,984 | 2,039 |
| Hold p50/p95/p99 | 11.49 / 18.19 / 27.35 ms | 11.65 / 17.70 / 26.54 ms |
| Release p50/p95/p99 | 7.21 / 11.90 / 17.35 ms | 7.32 / 11.78 / 16.45 ms |
| HTTP req/s | 4,870.4 | 4,900.4 |
| Pool max / 25 | 25 | 25 |
| Pool wait Δ | +527.035 s | +542.408 s |
| PostgreSQL CPU max/avg | 514.14% / 431.21% | 488.27% / 460.19% |
| Outbox lag max | 25.962 s | **1.109 s** |

Hot seat, 50 VUs, 30 s:

| Metric | Baseline | Adaptive |
| --- | --- | --- |
| Iterations | 151,338 (5,041.7/s) | 155,753 (5,188.7/s) |
| Successful cycles | 2,007 (66.9/s) | 2,077 (69.2/s) |
| Conflicts | 149,331 | 153,676 |
| Hold p50/p95/p99 | 8.56 / 17.98 / 23.52 ms | 8.35 / 17.36 / 22.39 ms |
| Release p50/p95/p99 | 12.74 / 19.30 / 22.98 ms | 12.27 / 18.46 / 22.09 ms |
| HTTP req/s | 5,108.6 | 5,257.9 |
| Pool max / 25 | 25 | 25 |
| Pool wait Δ | +487.632 s | +499.813 s |
| PostgreSQL CPU max/avg | 440.25% / 393.59% | 432.98% / 425.39% |
| Outbox lag max | 6.480 s | **0.063 s** |

Raw output:

- [uniform k6](raw/uniform-20261125T011422Z/k6.txt) · [monitor](raw/uniform-20261125T011422Z/monitor-summary.txt)
- [hot k6](raw/hot-20261125T011455Z/k6.txt) · [monitor](raw/hot-20261125T011455Z/monitor-summary.txt)

## Reading the result

- The uniform outbox lag is bounded by the 1 s idle period plus processing
  (~1.1 s). The hot lag is below 0.1 s.
- Hold throughput is unchanged or slightly higher within run-to-run noise
  (uniform +0.7%, hot +2.9%). p99 is unchanged to slightly better.
- Average PostgreSQL CPU is ~7-8% higher because the relay now processes the
  rows during the run instead of accumulating a 26 s backlog. That is the work
  the baseline deferred.
- The pool is still fully saturated in both write scenarios, and the hot seat is
  still limited to ~69 successful cycles/s by the row lock. The relay fix does
  not change those limits.
- The seat-map read path still queries the event row from PostgreSQL; the seat
  list itself is cached. That remains the next read-path experiment.

## Reproduction

```sh
docker compose up -d --build
BENCH_VUS=50 BENCH_DURATION=30s ./bench/run.sh uniform
BENCH_VUS=50 BENCH_DURATION=30s ./bench/run.sh hot
```

The runner resets the benchmark event and uses `docker compose run --no-deps` so
the API is not recreated by the k6 service's dependency on it.

## Caveats

- The tuning was measured on the same WSL2/Docker Desktop host as the baseline;
  the absolute values are comparable only there.
- The numbers are a single run per scenario, not a statistical distribution.
- The worker code change is in the working tree on top of `5e23710`; the raw
  `meta.txt` records the application commit, not a separate tuning commit.
