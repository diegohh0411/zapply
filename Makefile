SHELL := /bin/bash
COMPOSE ?= $(shell command -v podman >/dev/null && echo "podman compose" || echo "docker compose")
ENV_FILE ?= .env
ENV := set -a; [ -f $(ENV_FILE) ] && . ./$(ENV_FILE); set +a;

# Local Supabase (development only): Postgres 17, Auth, Studio and Mailpit in
# containers via the Supabase CLI. Podman serves the Docker API on its own socket.
SUPABASE ?= npx -y supabase@3.2.0
PODMAN_SOCK := $(shell podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}' 2>/dev/null)
SB := $(if $(PODMAN_SOCK),DOCKER_HOST=unix://$(PODMAN_SOCK) )$(SUPABASE)
SUPA_BACKUP := supabase/backup
# psql inside the local Supabase database container, so the host needs none.
# The local superuser: `postgres` can't hand objects to roles it isn't a member of.
SUPA_PSQL = $(firstword $(COMPOSE)) exec -i supabase_db_zapply psql -U supabase_admin -d postgres -v ON_ERROR_STOP=1

.PHONY: supa-up supa-down supa-pull supa-restore build run run-all worker relay migrate token db-up db-down firecrawl crawl4ai test test-integration vet image generate web web-dev

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
	$(COMPOSE) -f deploy/compose.yaml --profile firecrawl --profile crawl4ai down

firecrawl: ## Firecrawl Local on 127.0.0.1:3002, reading posting links (first build compiles Rust)
	@$(ENV) $(COMPOSE) -f deploy/compose.yaml --profile firecrawl up -d --build firecrawl

crawl4ai: ## Crawl4AI on 127.0.0.1:11235, a stealth-browser scraper provider (CRAWL4AI_TOKEN optional)
	@$(ENV) $(COMPOSE) -f deploy/compose.yaml --profile crawl4ai up -d crawl4ai

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

supa-up: ## Dev: local Supabase (Auth :54321, Postgres :54322, Studio :54323, Mailpit :54324)
	@# Generated outside the repo: here the CLI would try to read the missing file.
	@grep -qs '"kty"' supabase/signing_keys.json || { \
		key=$$(cd "$$(mktemp -d)" && $(SB) gen signing-key --algorithm ES256 </dev/null 2>/dev/null | head -1); \
		case "$$key" in '{"kty"'*) ;; *) echo "could not generate a signing key" >&2; exit 1;; esac; \
		umask 077 && echo "[$$key]" > supabase/signing_keys.json; }
	$(SB) start

supa-down: ## Dev: stop local Supabase (its data is kept)
	$(SB) stop

supa-pull: ## Dev: dump the hosted project (MIGRATE_DATABASE_URL in .env) into supabase/backup
	@mkdir -p $(SUPA_BACKUP) && chmod 700 $(SUPA_BACKUP)
	@$(ENV) url="$$MIGRATE_DATABASE_URL"; \
	case "$$url" in *supabase.com*) ;; *) echo "MIGRATE_DATABASE_URL in $(ENV_FILE) is not a hosted Supabase URL" >&2; exit 1;; esac; \
	$(SB) db dump --remote="$$url" -f $(SUPA_BACKUP)/roles.sql --role-only && \
	$(SB) db dump --remote="$$url" -f $(SUPA_BACKUP)/schema.sql && \
	$(SB) db dump --remote="$$url" -f $(SUPA_BACKUP)/data.sql --use-copy --data-only
	@ls -lh $(SUPA_BACKUP)

supa-restore: ## Dev: load supabase/backup into local Supabase (after `make supa-up`)
	@for f in roles schema data; do [ -s $(SUPA_BACKUP)/$$f.sql ] || { echo "missing $(SUPA_BACKUP)/$$f.sql: run make supa-pull" >&2; exit 1; }; done
	@{ echo 'BEGIN;'; \
	   cat $(SUPA_BACKUP)/roles.sql $(SUPA_BACKUP)/schema.sql; \
	   echo 'SET session_replication_role = replica;'; \
	   sed 's/^SET transaction_timeout/-- &/' $(SUPA_BACKUP)/data.sql | \
	     awk '/^COPY "storage"\./{skip=1} !skip{print} skip && /^\\\.$$/{skip=0}'; \
	   echo 'SET session_replication_role = origin;'; \
	   cat deploy/supabase/local-roles.sql; \
	   echo 'COMMIT;'; } | $(SUPA_PSQL)
	@echo "Restored. Run the app against it: make run-all ENV_FILE=.env.supabase"
