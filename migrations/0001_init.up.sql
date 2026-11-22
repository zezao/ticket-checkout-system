CREATE TYPE seat_status    AS ENUM ('available', 'held', 'sold');
CREATE TYPE hold_status    AS ENUM ('active', 'payment_pending', 'confirmed',
                                'expired', 'released', 'payment_failed');
CREATE TYPE payment_status AS ENUM ('created', 'pending', 'succeeded', 'failed');

CREATE TABLE events (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    venue      text        NOT NULL,
    starts_at  timestamptz NOT NULL,
    currency   char(3)     NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE seats (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id        uuid        NOT NULL REFERENCES events(id),
    section         text        NOT NULL,
    row_label       text        NOT NULL,
    number          int         NOT NULL,
    price_cents     bigint      NOT NULL CHECK (price_cents > 0),
    status          seat_status NOT NULL DEFAULT 'available',
    current_hold_id uuid,
    hold_expires_at timestamptz,
    UNIQUE (event_id, section, row_label, number),
    CHECK ((status = 'available') = (current_hold_id IS NULL)),
    CHECK (status = 'held' OR hold_expires_at IS NULL)
);
CREATE INDEX seats_event_idx ON seats (event_id);

CREATE TABLE holds (
    id           uuid PRIMARY KEY,
    seat_id      uuid        NOT NULL REFERENCES seats(id),
    user_id      uuid        NOT NULL,
    status       hold_status NOT NULL DEFAULT 'active',
    expires_at   timestamptz NOT NULL,
    amount_cents bigint      NOT NULL CHECK (amount_cents > 0),
    currency     char(3)     NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, seat_id)
);

ALTER TABLE seats ADD CONSTRAINT seats_current_hold_matches_seat_fk
    FOREIGN KEY (current_hold_id, id) REFERENCES holds (id, seat_id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE UNIQUE INDEX holds_one_live_per_seat ON holds (seat_id)
    WHERE status IN ('active', 'payment_pending', 'confirmed');
CREATE INDEX holds_expiry_idx ON holds (expires_at) WHERE status = 'active';

CREATE TABLE payment_attempts (
    id              uuid PRIMARY KEY,
    hold_id         uuid           NOT NULL UNIQUE REFERENCES holds(id),
    user_id         uuid           NOT NULL,
    idempotency_key text           NOT NULL,
    amount_cents    bigint         NOT NULL CHECK (amount_cents > 0),
    currency        char(3)        NOT NULL,
    status          payment_status NOT NULL DEFAULT 'created',
    attempts        int            NOT NULL DEFAULT 0,
    next_check_at   timestamptz    NOT NULL DEFAULT now(),
    deadline_at     timestamptz    NOT NULL,
    last_error      text,
    traceparent     text,
    created_at      timestamptz    NOT NULL DEFAULT now(),
    updated_at      timestamptz    NOT NULL DEFAULT now(),
    UNIQUE (user_id, idempotency_key),
    UNIQUE (id, hold_id)
);
CREATE INDEX payments_due_idx ON payment_attempts (next_check_at)
    WHERE status IN ('created', 'pending');

CREATE TABLE orders (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hold_id            uuid        NOT NULL UNIQUE,
    seat_id            uuid        NOT NULL UNIQUE,
    payment_attempt_id uuid        NOT NULL UNIQUE,
    user_id            uuid        NOT NULL,
    amount_cents       bigint      NOT NULL,
    currency           char(3)     NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (hold_id, seat_id) REFERENCES holds (id, seat_id),
    FOREIGN KEY (payment_attempt_id, hold_id) REFERENCES payment_attempts (id, hold_id)
);

CREATE TABLE outbox (
    id            bigserial PRIMARY KEY,
    topic         text        NOT NULL,
    payload       jsonb       NOT NULL,
    traceparent   text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    claimed_until timestamptz,
    published_at  timestamptz
);
CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL;
