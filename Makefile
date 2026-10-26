SHELL := /bin/bash
COMPOSE ?= $(shell command -v podman >/dev/null && echo "podman compose" || echo "docker compose")
ENV := set -a; [ -f .env ] && . ./.env; set +a;

.PHONY: build run migrate token db-up db-down test test-integration vet image generate

build:
	go build -o bin/zapply ./cmd/zapply

run: build ## API on the host, against `make db-up`
	@$(ENV) bin/zapply api

migrate: build
	@$(ENV) bin/zapply migrate

token: build ## Dev bearer token: make token SUB=alice
	@$(ENV) bin/zapply devtoken -sub $${SUB:-dev-user}

db-up:
	$(COMPOSE) -f deploy/compose.yaml up -d --wait

db-down:
	$(COMPOSE) -f deploy/compose.yaml down

generate: ## Regenerate sqlc queries and the OpenAPI server
	go tool sqlc generate
	go tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml

vet:
	gofmt -l . | (! grep .) && go vet ./...

test:
	go test -race ./...

test-integration:
	scripts/test-integration.sh

image:
	$(firstword $(COMPOSE)) build -t zapply .
