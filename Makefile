SHELL := /bin/sh

GO ?= go
GOLANGCI_LINT ?= golangci-lint
DATABASE_URL ?= postgres://tickets:tickets@localhost:5432/tickets?sslmode=disable

.PHONY: build run-api run-worker test test-unit test-integration lint fmt vet tidy \
	compose-up compose-down migrate migrate-down seed

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
