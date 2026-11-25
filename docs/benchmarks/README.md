# Benchmarks

These benches measure the reservation core and the cached read path. They set no
pass/fail thresholds: record a baseline, change one thing, and compare.

## Scenarios

| Scenario | Seat choice | What it stresses |
| --- | --- | --- |
| `hot` | every VU uses one seat | row-lock contention, pool saturation, p99 tail |
| `uniform` | random seat from 1,000 | general hold/release path with little contention |
| `skewed` | 80% of requests on 10% of seats | mixed contention |
| `seatmap` | read-only seat map | Redis cache-aside hits and API read path |

Every hold is released immediately by its owner, so the loop measures
acquire + release rather than a one-shot sell-out. Each iteration uses a fresh
user, so the per-user rate limit is not the limiting factor.

The benchmark event is `0199b000-0000-7000-8000-000000000001` with 1,000 seats,
created by `bench/bench_seed.sql`. The reset is scoped to that event.

## How to run

Start the stack first:

```sh
docker compose up -d --build
```

Run one scenario:

```sh
BENCH_VUS=50 BENCH_DURATION=30s ./bench/run.sh hot
BENCH_VUS=50 BENCH_DURATION=30s ./bench/run.sh uniform
BENCH_VUS=50 BENCH_DURATION=30s ./bench/run.sh skewed
BENCH_VUS=50 BENCH_DURATION=30s ./bench/run.sh seatmap
```

Or all four:

```sh
BENCH_VUS=50 BENCH_DURATION=30s make bench
```

Each run:

1. Stops the worker.
2. Resets the benchmark event and deletes its Redis seat-map key.
3. Restarts the worker.
4. Starts a monitor that samples `pgxpool_acquired_conns{pool="app"}`,
   cumulative pool wait, `outbox_lag_seconds` and PostgreSQL container CPU.
5. Runs k6 in the Compose network against `http://api:8080`.
6. Writes `meta.txt`, `monitor.csv`, `monitor-summary.txt` and `k6.txt` under
   `docs/benchmarks/raw/<scenario>-<UTC timestamp>/`.

The worker is stopped during the reset to avoid racing an outbox lease against
the deleted rows.

## What is recorded

From k6:

- `hold_duration` and `release_duration` percentiles (p50/p95/p99).
- `hold_success`, `hold_conflict` (409), `hold_errors` (unexpected statuses).
- `http_req_failed` with 200/201/204/409 treated as expected.
- `iterations/s` and `http_reqs/s`.

From the monitor:

- `pool_max_acquired` out of `PG_MAX_CONNS` (25).
- `pool_wait_seconds_delta`, the increase in cumulative acquire wait.
- `outbox_lag_max_seconds` from the worker metrics endpoint.
- `postgres_cpu_max_percent` and `postgres_cpu_avg_percent` from `docker stats`.

Environment: AMD Ryzen 7 8700G (8 cores, 16 threads), 15 GiB RAM, Docker 29.7.2
on WSL2, Go 1.27.3, k6 2.4.0, PostgreSQL 17-alpine, Redis 7-alpine. The API,
worker, PostgreSQL, Redis and k6 all run in containers on the same host.

## Reading the results

- `409` is expected contention, not an error. The unexpected error rate is
  `hold_errors / hold_outcomes`.
- In the hot scenario the row lock serializes winners; expect a low success
  count, a high conflict count and a heavy tail.
- Each successful hold and release writes a `seat.changed` outbox row. The relay
  processes at most 100 rows per second, so write-heavy scenarios can show
  growing `outbox_lag_seconds`. That is a real bottleneck and a tuning candidate,
  not a correctness problem.
- The Redis rate limiter adds one round trip per hold; it is included in the
  numbers.
- Cache metrics for the seatmap scenario are on the API `/metrics` endpoint
  (`seatmap_cache_total`).

## Caveats

- Docker Desktop/WSL2 shares CPU and memory with the host; absolute numbers are
  only comparable on the same machine.
- The k6 load generator competes with the API and databases for CPU.
- The mock provider is not involved in the hold/seatmap scenarios.
- Each successful payment in future payment benchmarks would add worker
  settlement work; the current scenarios do not pay.

## Runs

- [Baseline, 2026-11-24](2026-11-24-baseline.md)
