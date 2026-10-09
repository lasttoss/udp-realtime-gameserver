SHELL := /bin/bash
GO ?= go
COMPOSE ?= docker compose
CCU ?= 100
RAMP ?= 5s
DURATION ?= 20s

.PHONY: help build run load test race vet lint coverage smoke up down logs clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## Build the relay and the load generator into bin/
	$(GO) build -o bin/relay ./cmd/relay
	$(GO) build -o bin/botswarm ./cmd/botswarm

run: ## Run the relay locally (UDP :9000, HTTP :8080)
	$(GO) run ./cmd/relay

load: ## Load test a running relay: make load CCU=500 DURATION=60s
	$(GO) run ./cmd/botswarm -ccu $(CCU) -ramp $(RAMP) -duration $(DURATION)

test: ## Unit tests
	$(GO) test ./...

race: ## Unit tests with the race detector (the relay is a concurrent service)
	$(GO) test -race ./...

vet: ## Static analysis
	$(GO) vet ./...

fmt: ## Format
	$(GO) fmt ./...

lint: fmt vet ## Format + vet

coverage: ## Coverage report
	$(GO) test -coverprofile=coverage.out ./... && $(GO) tool cover -func=coverage.out | tail -1

up: ## Start the relay container, then smoke test it
	$(COMPOSE) up -d --build
	@$(MAKE) --no-print-directory smoke

down: ## Stop the container
	$(COMPOSE) down

logs: ## Tail the container log
	$(COMPOSE) logs -f relay

smoke: ## End-to-end smoke test against a running relay
	@PORT=$$(grep -E '^HTTP_PORT=' .env 2>/dev/null | cut -d= -f2); \
	UDP=$$(grep -E '^RELAY_PORT=' .env 2>/dev/null | cut -d= -f2); \
	python3 scripts/smoke.py 127.0.0.1 $${PORT:-8080} 127.0.0.1 $${UDP:-9000}

clean: ## Remove build output
	rm -rf bin coverage.out
