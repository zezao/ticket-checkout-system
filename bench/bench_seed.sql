BEGIN;

DELETE FROM outbox
 WHERE payload->>'seat_id' IN (
     SELECT id::text FROM seats
      WHERE event_id = '0199b000-0000-7000-8000-000000000001'
 )
    OR payload->>'event_id' = '0199b000-0000-7000-8000-000000000001';

DELETE FROM orders
 WHERE seat_id IN (
     SELECT id FROM seats
      WHERE event_id = '0199b000-0000-7000-8000-000000000001'
 );

DELETE FROM payment_attempts
 WHERE hold_id IN (
     SELECT h.id
       FROM holds h
       JOIN seats s ON s.id = h.seat_id
      WHERE s.event_id = '0199b000-0000-7000-8000-000000000001'
 );

DELETE FROM holds
 WHERE seat_id IN (
     SELECT id FROM seats
      WHERE event_id = '0199b000-0000-7000-8000-000000000001'
 );

DELETE FROM seats
 WHERE event_id = '0199b000-0000-7000-8000-000000000001';

DELETE FROM events
 WHERE id = '0199b000-0000-7000-8000-000000000001';

INSERT INTO events (id, name, venue, starts_at, currency)
VALUES (
    '0199b000-0000-7000-8000-000000000001',
    'Benchmark Night',
    'Load Test Arena',
    timestamptz '2027-09-01 19:00:00+00',
    'USD'
);

INSERT INTO seats (event_id, section, row_label, number, price_cents)
SELECT '0199b000-0000-7000-8000-000000000001',
       'GA',
       chr(64 + ((g - 1) / 50 + 1)::int),
       ((g - 1) % 50) + 1,
       5000
  FROM generate_series(1, 1000) AS g;

COMMIT;
