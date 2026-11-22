SHELL := /bin/sh

GO ?= go
GOLANGCI_LINT ?= golangci-lint

.PHONY: build run-api run-worker test test-unit lint fmt vet tidy

build:
	$(GO) build -o bin/api ./cmd/api
	$(GO) build -o bin/worker ./cmd/worker

run-api:
	$(GO) run ./cmd/api

run-worker:
	$(GO) run ./cmd/worker

test:
	$(GO) test -race ./...

test-unit:
	$(GO) test -race -short ./...

lint:
	$(GOLANGCI_LINT) run

fmt:
	$(GOLANGCI_LINT) fmt

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy
