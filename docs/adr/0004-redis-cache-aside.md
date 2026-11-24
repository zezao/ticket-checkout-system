# 0004: Redis Cache-Aside and Fail-Open Limits

Status: Accepted (Phase 4)

## Context

Seat maps are read far more often than they change, and a few clients can hammer
the hold endpoint. Redis is well suited to both, but its replication is
asynchronous. It must never become the authority for seat ownership or payment
state.

## Decision

- **Seat map is cache-aside.** `seatmap:{event_id}` holds a compact JSON array of
  seat id, section, row, number, status and price. The TTL is 5 seconds.
  `singleflight` collapses concurrent misses so one request queries PostgreSQL.
- **The outbox invalidates.** A `seat.changed` outbox row is written in the same
  transaction as the hold/seat change. The worker relay claims it with
  `FOR UPDATE SKIP LOCKED` and a 30-second lease, runs the Redis delete outside
  any transaction, and marks the row published. Delivery is at least once, so
  the delete is idempotent. Failed deletes remain unpublished and retry when
  the lease lapses.
- **A stale cache is safe.** Readers may see an old `available` seat for up to
  the TTL or relay lag. The hold endpoint always checks PostgreSQL and returns
  409, so the cache can never sell a seat twice.
- **Rate limit is fail-open.** A fixed-window counter per user, implemented with
  a Lua `INCR`/`EXPIRE`, limits `POST /events/{id}/holds` to
  `HOLD_RATE_LIMIT` per minute. On a Redis error the request proceeds and
  `redis_errors_total` increments.
- **No correctness state in Redis.** Locks, holds, payment attempts and anything
  an invariant depends on stay in PostgreSQL. The hold and payment use cases take
  no cache port; only the seat-map read is decorated.

## Consequences

- Redis can disappear without breaking holds or payments. The seat map falls
  back to PostgreSQL, and the rate limiter stops limiting until Redis returns.
- The cache is not a throughput guarantee for the first read after an
  invalidation; the TTL and relay lag are the accepted staleness window.
- The outbox relay is the single producer of cache invalidation. If it stalls,
  the TTL still bounds staleness.
- Metrics `seatmap_cache_total{result}`, `redis_errors_total`, `outbox_lag_seconds`
  and `consistency_errors_total` make the failure modes visible.
