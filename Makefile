SHELL := /bin/sh

GO ?= go
GOLANGCI_LINT ?= golangci-lint
DATABASE_URL ?= postgres://tickets:tickets@localhost:5432/tickets?sslmode=disable
BENCH_EVENT_ID ?= 0199b000-0000-7000-8000-000000000001
BENCH_VUS ?= 50
BENCH_DURATION ?= 30s

.PHONY: build run-api run-worker test test-unit test-integration lint fmt vet tidy \
	compose-up compose-down migrate migrate-down seed hooks \
	bench bench-seed bench-hot bench-uniform bench-skewed bench-seatmap

build:
	$(GO) build -o bin/api ./cmd/api
	$(GO) build -o bin/worker ./cmd/worker

run-api:
	DATABASE_URL="$(DATABASE_URL)" $(GO) run ./cmd/api

run-worker:
	DATABASE_URL="$(DATABASE_URL)" $(GO) run ./cmd/worker

test:
	$(GO) test -race ./...

test-unit:
	$(GO) test -race -short ./...

test-integration:
	$(GO) test -race -count=1 ./test/integration/...

lint:
	$(GOLANGCI_LINT) run

fmt:
	$(GOLANGCI_LINT) fmt

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

hooks:
	git config core.hooksPath .githooks

compose-up:
	docker compose up --build

compose-down:
	docker compose down -v

migrate:
	docker compose run --rm migrate

migrate-down:
	docker compose run --rm migrate \
		-path=/migrations \
		-database="postgres://tickets:tickets@postgres:5432/tickets?sslmode=disable" \
		down 1

seed:
	docker compose run --rm seed

bench: bench-hot bench-uniform bench-skewed bench-seatmap

bench-seed:
	docker compose stop worker >/dev/null 2>&1 || true
	docker compose exec -T postgres psql -U tickets -d tickets -v ON_ERROR_STOP=1 < bench/bench_seed.sql >/dev/null
	docker compose exec -T redis redis-cli DEL "seatmap:$(BENCH_EVENT_ID)" >/dev/null 2>&1 || true
	docker compose start worker >/dev/null 2>&1 || true

bench-hot:
	BENCH_EVENT_ID=$(BENCH_EVENT_ID) BENCH_VUS=$(BENCH_VUS) BENCH_DURATION=$(BENCH_DURATION) ./bench/run.sh hot

bench-uniform:
	BENCH_EVENT_ID=$(BENCH_EVENT_ID) BENCH_VUS=$(BENCH_VUS) BENCH_DURATION=$(BENCH_DURATION) ./bench/run.sh uniform

bench-skewed:
	BENCH_EVENT_ID=$(BENCH_EVENT_ID) BENCH_VUS=$(BENCH_VUS) BENCH_DURATION=$(BENCH_DURATION) ./bench/run.sh skewed

bench-seatmap:
	BENCH_EVENT_ID=$(BENCH_EVENT_ID) BENCH_VUS=$(BENCH_VUS) BENCH_DURATION=$(BENCH_DURATION) ./bench/run.sh seatmap
