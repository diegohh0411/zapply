SHELL := /bin/bash
COMPOSE ?= $(shell command -v podman >/dev/null && echo "podman compose" || echo "docker compose")
ENV := set -a; [ -f .env ] && . ./.env; set +a;

.PHONY: build run run-all worker relay migrate token db-up db-down firecrawl test test-integration vet image generate web web-dev

build:
	go build -o bin/zapply ./cmd/zapply

run: build ## API on the host, against `make db-up`
	@$(ENV) bin/zapply api

run-all: build ## API, worker and relay in one process with embedded NATS
	@$(ENV) bin/zapply all

relay: build ## Outbox relay on the host, against `make db-up`
	@$(ENV) bin/zapply relay

worker: build ## Durable model execution (separate terminal/process)
	@$(ENV) bin/zapply worker

migrate: build
	@$(ENV) bin/zapply migrate

token: build ## Dev bearer token: make token SUB=alice
	@$(ENV) bin/zapply devtoken -sub $${SUB:-dev-user}

db-up:
	$(COMPOSE) -f deploy/compose.yaml up -d --wait

db-down:
	$(COMPOSE) -f deploy/compose.yaml --profile firecrawl down

firecrawl: ## Firecrawl Local on 127.0.0.1:3002, reading posting links (first build compiles Rust)
	@$(ENV) $(COMPOSE) -f deploy/compose.yaml --profile firecrawl up -d --build firecrawl

generate: ## Regenerate sqlc queries, the OpenAPI server and the web client types
	go tool sqlc generate
	go tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml
	cd web && npm run --silent gen:api

web: ## Build the web app into internal/webui/dist (embedded by the next go build)
	cd web && npm ci --no-audit --no-fund && npm run build
	find internal/webui/dist -mindepth 1 ! -name .gitkeep -delete
	cp -R web/dist/web/browser/. internal/webui/dist/

web-dev: ## Angular dev server on :4200, proxying /v1 to `make run`
	cd web && npm start

vet:
	gofmt -l . | (! grep .) && go vet ./...

test:
	go test -race ./...

test-integration:
	scripts/test-integration.sh

image:
	$(firstword $(COMPOSE)) build -t zapply .
