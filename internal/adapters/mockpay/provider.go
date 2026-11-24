package mockpay

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zezao/ticket-checkout-system/internal/app"
)

// Behavior selects what a new charge looks like.
type Behavior string

const (
	BehaviorSuccess            Behavior = "success"
	BehaviorDecline            Behavior = "decline"
	BehaviorTimeoutAfterCharge Behavior = "timeout_after_charge"
	BehaviorUnresolved         Behavior = "unresolved"
)

// ErrTimeout simulates a provider timeout after a charge was recorded.
var ErrTimeout = errors.New("mock provider: timeout after charge")

// Provider is the in-process mock payment provider.
type Provider struct {
	pool     *pgxpool.Pool
	behavior Behavior
}

// New builds a mock provider. behavior is one of the MOCK_BEHAVIOR values.
func New(pool *pgxpool.Pool, behavior string) (*Provider, error) {
	selected := Behavior(behavior)
	switch selected {
	case BehaviorSuccess, BehaviorDecline, BehaviorTimeoutAfterCharge, BehaviorUnresolved:
	default:
		return nil, fmt.Errorf("mockpay: unknown behavior %q", behavior)
	}
	return &Provider{pool: pool, behavior: selected}, nil
}

// CreateCharge records a charge once per key and reports its status.
func (p *Provider) CreateCharge(ctx context.Context, req app.ChargeRequest) (app.ChargeResult, error) {
	status, found, err := p.chargeStatus(ctx, req.IdempotencyKey)
	if err != nil {
		return app.ChargeResult{}, err
	}
	if found {
		return app.ChargeResult{Status: mapStatus(status)}, nil
	}

	status = p.initialStatus()
	inserted, err := p.insertCharge(ctx, req, status)
	if err != nil {
		return app.ChargeResult{}, err
	}
	if !inserted {
		status, found, err = p.chargeStatus(ctx, req.IdempotencyKey)
		if err != nil {
			return app.ChargeResult{}, err
		}
		if found {
			return app.ChargeResult{Status: mapStatus(status)}, nil
		}
		return app.ChargeResult{Status: app.ChargeUnknown}, nil
	}

	if p.behavior == BehaviorTimeoutAfterCharge {
		return app.ChargeResult{}, ErrTimeout
	}
	return app.ChargeResult{Status: mapStatus(status)}, nil
}

// GetCharge returns the charge for a key, or unknown if none exists.
func (p *Provider) GetCharge(ctx context.Context, req app.ChargeRequest) (app.ChargeResult, error) {
	status, found, err := p.chargeStatus(ctx, req.IdempotencyKey)
	if err != nil {
		return app.ChargeResult{}, err
	}
	if !found {
		return app.ChargeResult{Status: app.ChargeUnknown}, nil
	}
	return app.ChargeResult{Status: mapStatus(status)}, nil
}

// ChargeCount reports how many charges exist for a key.
func (p *Provider) ChargeCount(ctx context.Context, key string) (int, error) {
	var count int
	if err := p.pool.QueryRow(ctx,
		"SELECT count(*) FROM mockpay.charges WHERE idempotency_key = $1", key,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("mockpay count: %w", err)
	}
	return count, nil
}

func (p *Provider) initialStatus() string {
	switch p.behavior {
	case BehaviorDecline:
		return "declined"
	case BehaviorUnresolved:
		return "processing"
	default:
		return "succeeded"
	}
}

func (p *Provider) chargeStatus(ctx context.Context, key string) (string, bool, error) {
	var status string
	err := p.pool.QueryRow(ctx,
		"SELECT status FROM mockpay.charges WHERE idempotency_key = $1", key,
	).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("mockpay lookup: %w", err)
	}
	return status, true, nil
}

func (p *Provider) insertCharge(
	ctx context.Context,
	req app.ChargeRequest,
	status string,
) (bool, error) {
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO mockpay.charges (idempotency_key, status, amount_cents, currency)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		req.IdempotencyKey, status, req.AmountCents, req.Currency)
	if err != nil {
		return false, fmt.Errorf("mockpay insert: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func mapStatus(status string) app.ChargeStatus {
	switch status {
	case "succeeded":
		return app.ChargeSucceeded
	case "declined":
		return app.ChargeDeclined
	case "processing":
		return app.ChargeProcessing
	default:
		return app.ChargeUnknown
	}
}
