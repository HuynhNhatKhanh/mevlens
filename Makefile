SHELL := /bin/bash
CONFIG ?= configs/arbitrum-one.toml
COMPOSE := docker compose -f deploy/compose.yaml
CH_ENV  := MEVLENS_CH_ADDR=127.0.0.1:9000 MEVLENS_CH_USER=$${CLICKHOUSE_USER:-mevlens} MEVLENS_CH_PASSWORD=$${CLICKHOUSE_PASSWORD}
RUN_ENV := CLICKHOUSE_ADDR=$${CLICKHOUSE_ADDR:-127.0.0.1:9000} CLICKHOUSE_USER=$${CLICKHOUSE_USER:-mevlens}

.PHONY: help build test race itest bench lint vuln fmt cover up down follow inspect clean

help: ## Show targets
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

build: ## Build bin/mevlens
	go build -trimpath -o bin/mevlens ./cmd/mevlens

test: ## Unit + golden tests (offline, no Docker)
	go test ./...

race: ## Unit tests with the race detector
	go test -race -count=1 ./...

itest: ## Integration tests against the compose ClickHouse (requires CLICKHOUSE_PASSWORD)
	$(CH_ENV) go test -tags integration -count=1 ./internal/store/...

bench: ## Hot-path benchmarks
	go test -run '^$$' -bench . -benchmem ./internal/...

cover: ## Coverage report
	go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

lint: ## golangci-lint v2 (install: https://golangci-lint.run/welcome/install/)
	golangci-lint run ./...

vuln: ## Known-vulnerability scan
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

fmt: ## Format
	gofmt -w cmd internal

up: ## Start ClickHouse, VictoriaMetrics and Grafana
	$(COMPOSE) up -d

down: ## Stop the stack (data volumes are kept)
	$(COMPOSE) down

follow: build ## Follow the chain head into the local ClickHouse
	$(RUN_ENV) ./bin/mevlens follow -config $(CONFIG) -listen 0.0.0.0:9464

inspect: build ## Classify the latest block (no database needed)
	CLICKHOUSE_ADDR=unused CLICKHOUSE_USER=unused CLICKHOUSE_PASSWORD=unused ./bin/mevlens inspect -config $(CONFIG)

clean:
	rm -rf bin coverage.out
