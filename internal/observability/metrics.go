// Package observability wires metrics and tracing for the api and worker.
package observability

import (
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns this process's Prometheus collectors.
type Metrics struct {
	registry *prometheus.Registry

	holdAttempts        *prometheus.CounterVec
	holdDuration        prometheus.Histogram
	payments            *prometheus.CounterVec
	paymentsOverdue     prometheus.Gauge
	outboxLag           prometheus.Gauge
	invariantViolations prometheus.Counter
	consistencyErrors   prometheus.Counter
	seatmapCache        *prometheus.CounterVec
	redisErrors         prometheus.Counter
}

// NewMetrics builds a registry with the process and application collectors.
func NewMetrics() *Metrics {
	registry := prometheus.NewRegistry()
	metrics := &Metrics{
		registry: registry,
		holdAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hold_attempts_total",
			Help: "Hold requests by result.",
		}, []string{"result"}),
		holdDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "hold_acquire_duration_seconds",
			Help:    "Time spent acquiring a hold.",
			Buckets: prometheus.DefBuckets,
		}),
		payments: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "payments_total",
			Help: "Payment attempts and settlement outcomes by result.",
		}, []string{"result"}),
		paymentsOverdue: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "payments_overdue",
			Help: "Open payments past their deadline.",
		}),
		outboxLag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_lag_seconds",
			Help: "Age of the oldest unpublished outbox row.",
		}),
		invariantViolations: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "invariant_violations_total",
			Help: "Invariant violations reported by the checker.",
		}),
		consistencyErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "consistency_errors_total",
			Help: "Writes that rolled back because a dependent row count was unexpected.",
		}),
		seatmapCache: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "seatmap_cache_total",
			Help: "Seat-map cache outcomes.",
		}, []string{"result"}),
		redisErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "redis_errors_total",
			Help: "Redis operation failures.",
		}),
	}

	registry.MustRegister(
		metrics.holdAttempts,
		metrics.holdDuration,
		metrics.payments,
		metrics.paymentsOverdue,
		metrics.outboxLag,
		metrics.invariantViolations,
		metrics.consistencyErrors,
		metrics.seatmapCache,
		metrics.redisErrors,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return metrics
}

// Handler serves the registry in Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// HoldAttempt records one hold acquire attempt and its latency.
func (m *Metrics) HoldAttempt(result string, duration time.Duration) {
	m.holdAttempts.WithLabelValues(result).Inc()
	m.holdDuration.Observe(duration.Seconds())
}

// Payment records one payment event.
func (m *Metrics) Payment(result string) {
	m.payments.WithLabelValues(result).Inc()
}

// Overdue sets the current count of overdue payments.
func (m *Metrics) Overdue(count int) {
	m.paymentsOverdue.Set(float64(count))
}

// OutboxLag sets the age of the oldest unpublished outbox row.
func (m *Metrics) OutboxLag(lag time.Duration) {
	m.outboxLag.Set(lag.Seconds())
}

// InvariantViolation records one reported invariant violation.
func (m *Metrics) InvariantViolation() {
	m.invariantViolations.Inc()
}

// ConsistencyError records one rolled-back consistency failure.
func (m *Metrics) ConsistencyError() {
	m.consistencyErrors.Inc()
}

// SeatMapCache records one cache outcome: hit, miss or error.
func (m *Metrics) SeatMapCache(result string) {
	m.seatmapCache.WithLabelValues(result).Inc()
}

// RedisError records one failed Redis operation.
func (m *Metrics) RedisError() {
	m.redisErrors.Inc()
}

// RegisterPool exposes pgx pool saturation and cumulative acquire wait time.
func (m *Metrics) RegisterPool(
	name string,
	acquired func() int,
	waitSeconds func() float64,
) error {
	labels := prometheus.Labels{"pool": name}

	if err := m.registry.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "pgxpool_acquired_conns",
		Help:        "Connections currently acquired from the pool.",
		ConstLabels: labels,
	}, func() float64 {
		return float64(acquired())
	})); err != nil {
		return fmt.Errorf("register pgxpool acquired conns: %w", err)
	}

	if err := m.registry.Register(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name:        "pgxpool_acquire_wait_seconds_total",
		Help:        "Cumulative time spent waiting for a pool connection.",
		ConstLabels: labels,
	}, waitSeconds)); err != nil {
		return fmt.Errorf("register pgxpool wait: %w", err)
	}
	return nil
}
