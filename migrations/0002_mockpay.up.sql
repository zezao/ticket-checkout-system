CREATE SCHEMA mockpay;

CREATE TABLE mockpay.charges (
    idempotency_key text        PRIMARY KEY,
    status          text        NOT NULL CHECK (status IN ('succeeded', 'declined', 'processing')),
    amount_cents    bigint      NOT NULL,
    currency        char(3)     NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
