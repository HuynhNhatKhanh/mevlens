SHELL := /bin/bash
CONFIG ?= configs/arbitrum-one.toml
COMPOSE := docker compose -f deploy/compose.yaml
CH_ENV  := MEVLENS_CH_ADDR=127.0.0.1:9000 MEVLENS_CH_USER=$${CLICKHOUSE_USER:-mevlens} MEVLENS_CH_PASSWORD=$${CLICKHOUSE_PASSWORD}
RUN_ENV := CLICKHOUSE_ADDR=$${CLICKHOUSE_ADDR:-127.0.0.1:9000} CLICKHOUSE_USER=$${CLICKHOUSE_USER:-mevlens}

# Admin server of `make follow`. It is unauthenticated (pprof, execution traces),
# so it must not listen on the LAN. Default: the docker0 bridge address, which is
# what compose's host.docker.internal (host-gateway) resolves to on Linux, so
# Prometheus can still scrape the host process; 127.0.0.1 where there is no
# docker0 (Docker Desktop forwards host.docker.internal to the host's loopback).
# Override with e.g. `make follow LISTEN=127.0.0.1:9464`.
DOCKER_GW = $(shell ip -4 -o addr show dev docker0 2>/dev/null | awk '{sub("/.*", "", $$4); print $$4; exit}')
LISTEN   ?= $(or $(DOCKER_GW),127.0.0.1):9464

# promtool/amtool come from the very images compose runs (one place to bump them).
compose_image = $(shell awk '$$1 == "image:" && index($$2, "$(1):") == 1 {print $$2; exit}' deploy/compose.yaml)
PROMETHEUS_IMAGE   = $(call compose_image,prom/prometheus)
ALERTMANAGER_IMAGE = $(call compose_image,prom/alertmanager)

.PHONY: help build test race itest bench lint vuln fmt cover monitoring-check up down follow inspect clean

help: ## Show targets
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-17s %s\n", $$1, $$2}'

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

monitoring-check: ## Validate the Prometheus/Alertmanager config and unit-test the alert rules (Docker)
	docker run --rm -v "$(CURDIR)/deploy/prometheus:/etc/prometheus:ro" --entrypoint promtool $(PROMETHEUS_IMAGE) check config /etc/prometheus/prometheus.yml
	docker run --rm -v "$(CURDIR)/deploy/prometheus:/etc/prometheus:ro" --entrypoint promtool $(PROMETHEUS_IMAGE) test rules /etc/prometheus/tests/mevlens_test.yml
	docker run --rm -v "$(CURDIR)/deploy/alertmanager:/etc/alertmanager:ro" --entrypoint amtool $(ALERTMANAGER_IMAGE) check-config /etc/alertmanager/alertmanager.yml

up: ## Start ClickHouse, Prometheus, Alertmanager and Grafana
	$(COMPOSE) up -d --remove-orphans

down: ## Stop the stack (data volumes are kept)
	$(COMPOSE) down

follow: build ## Follow the chain head into the local ClickHouse (admin server on $LISTEN)
	$(RUN_ENV) ./bin/mevlens follow -config $(CONFIG) -listen $(LISTEN)

inspect: build ## Classify the latest block (no database needed)
	CLICKHOUSE_ADDR=unused CLICKHOUSE_USER=unused CLICKHOUSE_PASSWORD=unused ./bin/mevlens inspect -config $(CONFIG)

clean:
	rm -rf bin coverage.out
