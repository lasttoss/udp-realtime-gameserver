SHELL := /bin/bash
GO ?= go
COMPOSE ?= docker compose
CCU ?= 100
RAMP ?= 5s
DURATION ?= 20s

.PHONY: help build run load test race vet lint coverage smoke up down logs clean diagram

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

# Sources are HTML and Mermaid; a PNG is a build artifact.
diagram:
	@if command -v chromium >/dev/null 2>&1; then B=chromium; elif command -v google-chrome >/dev/null 2>&1; then B=google-chrome; else echo "no chromium on PATH: open docs/diagrams/*.html in a browser"; exit 0; fi; \
	for f in docs/diagrams/*.html; do $$B --headless --screenshot="$${f%.html}.png" --window-size=1200,1000 "$$f" && echo "wrote $${f%.html}.png"; done

# The chart is the deployable unit here, so it gets the same gate the app gets: strict lint,
# a render of both branches (defaults, and every optional feature on), and schema validation.
# CI runs the wider version of this job, including a render-time negative check; this target is
# the one a person runs before pushing. kubeconform is fetched into .cache/ on first use.
KUBECONFORM_VERSION = v0.6.7
KUBECONFORM = .cache/kubeconform

chart: ## Lint the chart, render every branch, validate the objects
	@helm lint --strict deploy/helm/udp-relay
	@mkdir -p /tmp/udp-chart && \
	  helm template arena deploy/helm/udp-relay > /tmp/udp-chart/default.yaml && \
	  helm template arena deploy/helm/udp-relay \
	    --set autoscaling.enabled=true \
	    --set prometheus.serviceMonitor.enabled=true \
	    --set networkPolicy.enabled=true \
	    --set service.udp.enabled=true \
	    --set http.hostPort.enabled=true \
	    --set secret.existingSecret=byo-secret \
	    --set advertisePodIP=false \
	    --set extraEnv[0].name=RELAY_ADVERTISE \
	    --set extraEnv[0].value=one.example.com:9000 \
	    --set extraEnv[1].name=RELAY_ADVERTISE \
	    --set extraEnv[1].value=two.example.com:9000 > /tmp/udp-chart/full.yaml
	@python3 scripts/check-render.py --expect-count RELAY_ADVERTISE=2 /tmp/udp-chart/full.yaml /tmp/udp-chart/default.yaml
	@if [ ! -x "$(KUBECONFORM)" ]; then \
	  echo "  fetching kubeconform $(KUBECONFORM_VERSION) into .cache/"; \
	  mkdir -p .cache && curl -sSLo .cache/kubeconform.tar.gz \
	    https://github.com/yannh/kubeconform/releases/download/$(KUBECONFORM_VERSION)/kubeconform-linux-amd64.tar.gz && \
	  tar -xzf .cache/kubeconform.tar.gz -C .cache kubeconform && chmod +x "$(KUBECONFORM)"; fi
	@for f in /tmp/udp-chart/default.yaml /tmp/udp-chart/full.yaml; do \
	  echo "  validating $$f"; \
	  "$(KUBECONFORM)" -strict -summary -ignore-missing-schemas -kubernetes-version 1.30.0 - < "$$f"; \
	done
.PHONY: chart
