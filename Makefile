.PHONY: build build-gateway build-admin build-nodeagent build-web \
        run-gateway run-admin run-nodeagent run-web web-install \
        docker-build docker-push docker-build-web \
        test lint \
        migrate migrate-external migrate-dry _check-dsn _run-migration-external \
        dev-up dev-up-gpu dev-down \
        generate-key \
        placement-simulate node-status \
        deploy-gemma2-2b pull-gguf runtime-status \
        project-list project-create project-priority project-reserve project-preemptions \
        clean

BINARY_DIR := bin
REGISTRY   ?= registry.internal/nexusllm
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GO_FLAGS   := -ldflags="-w -s -X main.Version=$(VERSION)"

# Helper: run one migration inside the running postgres container.
# Suppresses routine DDL noise but shows real errors.
define run_migration
	docker compose exec -T postgres psql -U nexus -d nexusllm \
	  -v ON_ERROR_STOP=1 \
	  -f /migrations/$(1) 2>&1 | \
	  grep -vE "^(COMMIT|BEGIN|ALTER TABLE|ALTER|CREATE INDEX|CREATE|DROP|DO|INSERT 0|NOTICE|SET|$$)" || true
endef

# Internal: run one SQL file against an external DB using a Docker psql client.
# Avoids requiring psql to be installed on the build machine.
define run_migration_external
	@echo "  → migrations/$(1)"
	@docker run --rm \
	  --network host \
	  -e PGPASSWORD="$$(echo $(DB_DSN) | sed 's|.*://[^:]*:\([^@]*\)@.*|\1|')" \
	  -v "$(CURDIR)/migrations:/migrations:ro" \
	  postgres:15-alpine \
	  psql "$(DB_DSN)" -f /migrations/$(1) -v ON_ERROR_STOP=1 \
	  2>&1 | grep -vE "^(COMMIT|BEGIN|ALTER|CREATE|DROP|INSERT 0|NOTICE|SET|$$)" || true
endef

# ─────────────────────────────────────────────────────────────────────────────
# Build
# ─────────────────────────────────────────────────────────────────────────────
build: build-gateway build-admin build-nodeagent

build-gateway:
	@echo "→ Building nexus-gateway..."
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 go build $(GO_FLAGS) -o $(BINARY_DIR)/nexus-gateway ./cmd/gateway

build-admin:
	@echo "→ Building nexus-admin..."
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 go build $(GO_FLAGS) -o $(BINARY_DIR)/nexus-admin ./cmd/admin

build-nodeagent:
	@echo "→ Building nexus-nodeagent..."
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 go build $(GO_FLAGS) -o $(BINARY_DIR)/nexus-nodeagent ./cmd/nodeagent

# Build the Admin Web UI production image (requires Docker)
# Usage: make build-web
# Override the admin URL at build time: make build-web NEXUS_ADMIN_URL=http://admin:8081/admin/v1
build-web:
	@echo "→ Building nexus-web Docker image..."
	docker build \
	  --build-arg NEXUS_ADMIN_URL=$(or $(NEXUS_ADMIN_URL),http://localhost:8081/admin/v1) \
	  -f Dockerfile.web \
	  -t nexusllm/web:$(VERSION) \
	  .
	@echo "✓ Image: nexusllm/web:$(VERSION)"
	@echo "  Run: docker run -p 3000:3000 -e NEXUS_ADMIN_URL=http://admin:8081/admin/v1 nexusllm/web:$(VERSION)"

# ─────────────────────────────────────────────────────────────────────────────
# Run locally (postgres + redis must be running via make dev-up)
# ─────────────────────────────────────────────────────────────────────────────
run-gateway: build-gateway
	NEXUS_SERVER_MODE=debug \
	NEXUS_DATABASE_DSN="postgres://nexus:nexus@localhost:5432/nexusllm?sslmode=disable" \
	NEXUS_REDIS_ADDR="localhost:6379" \
	NEXUS_AUTH_JWTSECRET="dev-secret" \
	./$(BINARY_DIR)/nexus-gateway

run-admin: build-admin
	NEXUS_ADMIN_PORT=8081 \
	NEXUS_SERVER_MODE=debug \
	NEXUS_DATABASE_DSN="postgres://nexus:nexus@localhost:5432/nexusllm?sslmode=disable" \
	NEXUS_REDIS_ADDR="localhost:6379" \
	NEXUS_AUTH_JWTSECRET="dev-secret" \
	./$(BINARY_DIR)/nexus-admin

# Run the node agent — uses new task-based system with JWT auth.
# On first run it auto-registers and saves credentials to /var/lib/nexus-agent/.
# On remote nodes, set NEXUS_ADMIN_URL=http://<control-plane-ip>:8081
run-nodeagent: build-nodeagent
	NEXUS_ADMIN_URL="http://localhost:8081" \
	NEXUS_AGENT_INTERVAL="30s" \
	NEXUS_HEARTBEAT_INTERVAL="15s" \
	NEXUS_TASK_WORKERS="4" \
	NEXUS_LOG_LEVEL="info" \
	./$(BINARY_DIR)/nexus-nodeagent

run-web:
	@echo "→ Starting Web Admin UI on http://localhost:3001"
	@cd web && npm run dev

web-install:
	@cd web && npm install --legacy-peer-deps

# ─────────────────────────────────────────────────────────────────────────────
# Test & Lint
# ─────────────────────────────────────────────────────────────────────────────
test:
	go test ./... -v -race -timeout 120s

lint:
	@which golangci-lint > /dev/null || \
	  (echo "install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest" && exit 1)
	golangci-lint run ./...

# ─────────────────────────────────────────────────────────────────────────────
# Docker
# ─────────────────────────────────────────────────────────────────────────────
docker-build:
	docker build -f Dockerfile.gateway  --network host  --build-arg HTTP_PROXY=http://127.0.0.1:3000 --build-arg HTTPS_PROXY=http://127.0.0.1:3000  -t $(REGISTRY)/gateway:$(VERSION)   .
	docker build -f Dockerfile.admin    --network host  --build-arg HTTP_PROXY=http://127.0.0.1:3000 --build-arg HTTPS_PROXY=http://127.0.0.1:3000  -t $(REGISTRY)/admin:$(VERSION)     .
# 	docker build -f Dockerfile.nodeagent  --network host  --build-arg HTTP_PROXY=http://127.0.0.1:3000 --build-arg HTTPS_PROXY=http://127.0.0.1:3000 -t $(REGISTRY)/nodeagent:$(VERSION) .
	docker build -f Dockerfile.web      --network host  --build-arg HTTP_PROXY=http://127.0.0.1:3000 --build-arg HTTPS_PROXY=http://127.0.0.1:3000  -t $(REGISTRY)/web:$(VERSION)       .

docker-push: docker-build
	docker push $(REGISTRY)/gateway:$(VERSION)
	docker push $(REGISTRY)/admin:$(VERSION)
# 	docker push $(REGISTRY)/nodeagent:$(VERSION)
	docker push $(REGISTRY)/web:$(VERSION)

# ─────────────────────────────────────────────────────────────────────────────
# Database migrations
# IMPORTANT: files are applied in the EXPLICIT order listed in MIGRATIONS below.
# Do NOT rely on ls | sort — several filenames share the same numeric prefix
# (e.g. 005_ai_platform depends on 005_enterprise_platform) and alphabetical
# order breaks those dependencies.
#
# When adding a new migration:
#   1. Create the .sql file in migrations/
#   2. Append its filename at the END of the MIGRATIONS list below.
# ─────────────────────────────────────────────────────────────────────────────
MIGRATIONS := \
	001_initial.sql \
	002_seed_data.sql \
	003_runtime_layer.sql \
	004_single_gpu_runtime_seed.sql \
	005_enterprise_platform.sql \
	005_ai_platform.sql \
	006_controller_columns.sql \
	006_h200_platform_seed.sql \
	007_agent_tasks.sql \
	008_node_model_cache.sql \
	009_resilience.sql \
	010_lazy_runtime.sql \
	011_projects.sql \
	011_runtime_config_gpu.sql \
	012_unified_startup_states.sql \
	013_start_model_task_type.sql \
	014_execution_mode.sql \
	015_catchup_schema.sql \
	016_workload_policy.sql \
	017_scheduler.sql \
	018_weighted_priority.sql \
	018b_catchup_weighted.sql \
	018b_weighted_priority_fixup.sql \
	019_ha_replicas.sql \
	020_port_allocator.sql \
	021_missing_columns.sql \
	022_project_api_keys.sql \
	023_project_policies.sql \
	024_placement_v2.sql \
	025_placement_labels.sql \
	026_extra_args.sql \
	027_thinking_mode.sql \
	028_replica_slot_guard.sql \
	029_rolling_replacement.sql \
	030_state_constraint_catchup.sql \
	031_org_as_root.sql \
	032_fix_claim_replica_slot_failed_state.sql \
	033_universal_runtime_platform.sql \
	034_deprecate_legacy_columns.sql \
	035_drop_deprecated_columns.sql \
	036_drop_service_abstraction_remnants.sql \
	037_capability_validation.sql \
	038_recover_stuck_runtimes.sql \
	039_register_orphan_gpt_oss_120b.sql

migrate:
	@echo "→ Waiting for postgres..."
	@until docker compose exec -T postgres pg_isready -U nexus -d nexusllm > /dev/null 2>&1; \
	  do printf '.'; sleep 2; done; echo ""
	@echo "→ Copying migrations into container..."
	@docker compose cp migrations/. postgres:/migrations/
	@echo "→ Applying all migrations in order..."
	@for f in $(MIGRATIONS); do \
	  echo "  → $$f"; \
	  docker compose exec -T postgres psql -U nexus -d nexusllm \
	    -v ON_ERROR_STOP=1 -f /migrations/$$f \
	    2>&1 | grep -vE "^(COMMIT|BEGIN|ALTER|CREATE|DROP|INSERT 0|NOTICE|SET|DO|$$)" || true; \
	done
	@echo "✓ All migrations complete"

# ─────────────────────────────────────────────────────────────────────────────
# External DB migrations
# Use when postgres is NOT running in docker-compose (e.g. RDS, CloudSQL,
# managed Postgres, or a separate server).
#
# Required env var:
#   DB_DSN — full Postgres connection string
#            postgres://user:pass@host:5432/nexusllm?sslmode=require
#
# Works without a local psql install — uses the Docker postgres image as the
# psql client. All migration files in migrations/ are applied in sort order.
#
# Usage:
#   make migrate-external DB_DSN="postgres://nexus:secret@10.0.0.5:5432/nexusllm"
#   make migrate-dry                          # list files without connecting
# ─────────────────────────────────────────────────────────────────────────────
DB_DSN ?=

_check-dsn:
	@test -n "$(DB_DSN)" || { \
	  echo ""; \
	  echo "ERROR: DB_DSN is required."; \
	  echo ""; \
	  echo "  Usage: make migrate-external DB_DSN=\"postgres://user:pass@host:5432/db\""; \
	  echo ""; \
	  exit 1; }

migrate-external: _check-dsn
	@echo "→ Migrating external DB: $(DB_DSN)"
	@echo "  Using Docker postgres:15-alpine psql client (no local psql required)"
	@echo ""
	@echo "  Testing connection (10s timeout)..."
	@docker run --rm --network host \
	  -e PGPASSWORD="$$(echo '$(DB_DSN)' | sed 's|.*://[^:]*:\([^@]*\)@.*|\1|')" \
	  -e PGCONNECT_TIMEOUT=10 \
	  postgres:15-alpine \
	  psql "$(DB_DSN)" \
	    --no-password \
	    -c "SELECT 1;" \
	    -t -q \
	  > /dev/null 2>&1 \
	  || { echo ""; echo "ERROR: Cannot connect to $(DB_DSN)"; echo "       Check DB_DSN, network, and credentials."; echo ""; exit 1; }
	@echo "  ✓ Connected"
	@echo ""
	@PGPASSWORD="$$(echo '$(DB_DSN)' | sed 's|.*://[^:]*:\([^@]*\)@.*|\1|')"; \
	for f in $(MIGRATIONS); do \
	  echo "  → $$f"; \
	  docker run --rm \
	    --network host \
	    -e PGPASSWORD="$$PGPASSWORD" \
	    -e PGCONNECT_TIMEOUT=10 \
	    -v "$(CURDIR)/migrations:/migrations:ro" \
	    postgres:15-alpine \
	    psql "$(DB_DSN)" \
	      --no-password \
	      -v ON_ERROR_STOP=1 \
	      -f /migrations/$$f \
	    2>&1 | grep -vE "^(COMMIT|BEGIN|ALTER|CREATE|DROP|INSERT 0|NOTICE|SET|DO|[-]+|$$)" || true; \
	done
	@echo ""
	@echo "✓ All migrations complete on external DB"

# Internal target kept for backward compat but no longer used by the loop above.
_run-migration-external:
	@PGPASSWORD="$$(echo '$(DB_DSN)' | sed 's|.*://[^:]*:\([^@]*\)@.*|\1|')"; \
	docker run --rm \
	  --network host \
	  -e PGPASSWORD="$$PGPASSWORD" \
	  -e PGCONNECT_TIMEOUT=10 \
	  -v "$(CURDIR)/migrations:/migrations:ro" \
	  postgres:15-alpine \
	  psql "$(DB_DSN)" --no-password -v ON_ERROR_STOP=1 -f /migrations/$(FILE) \
	  2>&1 | grep -vE "^(COMMIT|BEGIN|ALTER|CREATE|DROP|INSERT 0|NOTICE|SET|DO|[-]+|$$)" || true

# Dry-run: print the SQL files that would be applied without connecting
migrate-dry:
	@echo "→ Migrations that would be applied (in order):"
	@for f in $(MIGRATIONS); do echo "  migrations/$$f"; done
	@echo ""
	@echo "To run against an external DB:"
	@echo "  make migrate-external DB_DSN=\"postgres://user:pass@host:5432/nexusllm\""
	@echo ""
	@echo "To run against local docker-compose postgres:"
	@echo "  make migrate"
	@echo ""
	@echo "To run a single migration manually:"
	@echo "  docker run --rm --network host -v \$$(pwd)/migrations:/m postgres:15-alpine \\"
	@echo "    psql \"\$$DB_DSN\" -f /m/029_rolling_replacement.sql"

# ─────────────────────────────────────────────────────────────────────────────
# Local dev stack
# ─────────────────────────────────────────────────────────────────────────────
dev-up:
	@echo "→ Starting postgres + redis..."
	docker compose up -d postgres redis
	$(MAKE) migrate
	@echo ""
	@echo "✓ Dev stack ready — run services:"
	@echo "  make run-gateway    →  http://localhost:8080  (inference)"
	@echo "  make run-admin      →  http://localhost:8081  (control plane + scheduler)"
	@echo "  make run-nodeagent  →  (on each GPU/CPU server)"
	@echo ""
	@echo "AI Platform endpoints (gateway):"
	@echo "  POST /v1/chat/completions"
	@echo "  POST /v1/embeddings"
	@echo "  POST /v1/rerank"
	@echo "  POST /v1/audio/transcriptions"
	@echo "  POST /v1/audio/speech"
	@echo "  POST /v1/ocr"

dev-up-gpu:
	@test -n "$(HF_TOKEN)" || (echo "ERROR: export HF_TOKEN=hf_..." && exit 1)
	docker compose -f docker-compose.single-gpu.yml up -d postgres redis
	$(MAKE) migrate
	docker compose -f docker-compose.single-gpu.yml up -d
	@echo "✓ GPU stack started"

dev-down:
	docker compose down -v 2>/dev/null || true
	docker compose -f docker-compose.single-gpu.yml down -v 2>/dev/null || true

# ─────────────────────────────────────────────────────────────────────────────
# AI Platform shortcuts (require admin to be running)
# ─────────────────────────────────────────────────────────────────────────────
ADMIN_URL ?= http://localhost:8081/admin/v1

# Simulate placement for a model — usage: make placement-simulate MODEL=qwen3-32b VRAM=65536 GPUS=1
placement-simulate:
	curl -s -X POST $(ADMIN_URL)/placement/simulate \
	  -H 'Content-Type: application/json' \
	  -d '{"model_name":"$(MODEL)","runtime_type":"GPU_RUNTIME","min_vram_mb":$(VRAM),"gpu_count":$(GPUS)}' | jq .

# Show node status
node-status:
	curl -s $(ADMIN_URL)/nodes | jq '.data[] | {hostname, status, total_cpu, total_ram_mb, total_vram_mb, last_heartbeat_at}'

# ─────────────────────────────────────────────────────────────────────────────
# Utilities
# ─────────────────────────────────────────────────────────────────────────────
generate-key:
	go run ./tools/genkey/main.go

# ─────────────────────────────────────────────────────────────────────────────
# llama.cpp model shortcuts
# Deploy gemma-2-2b on a node:
#   make deploy-gemma2-2b NODE_ID=<id> PORT=8090
# ─────────────────────────────────────────────────────────────────────────────
NODE_ID  ?= $(shell curl -s $(ADMIN_URL)/nodes | jq -r '.data[0].id')
PORT     ?= 8090

deploy-gemma2-2b:
	@test -n "$(NODE_ID)" || (echo "ERROR: NODE_ID not set" && exit 1)
	$(eval MODEL_ID := $(shell curl -s -X POST $(ADMIN_URL)/models/deploy \
	  -H 'Content-Type: application/json' \
	  -d '{"name":"gemma2-2b","display_name":"Gemma 2 2B","backend_type":"llamacpp","image":"ghcr.io/ggml-org/llama.cpp:server","host":"localhost","port":$(PORT),"node_id":"$(NODE_ID)","start_now":false}' \
	  | jq -r '.model_id'))
	@echo "→ Model registered: $(MODEL_ID)"
	@curl -s -X PUT $(ADMIN_URL)/models/$(MODEL_ID)/lazy-config \
	  -H 'Content-Type: application/json' \
	  -d '{"hf_repo":"bartowski/gemma-2-2b-it-GGUF","hf_file":"gemma-2-2b-it-Q4_K_M.gguf","ctx_size":8192,"n_gpu_layers":-1,"idle_timeout_secs":900}' | jq .
	@echo "→ Dispatching PULL_MODEL to node $(NODE_ID)..."
	@curl -s -X POST $(ADMIN_URL)/nodes/$(NODE_ID)/tasks \
	  -H 'Content-Type: application/json' \
	  -d '{"task_type":"PULL_MODEL","priority":80,"actor":"makefile","payload":{"model_id":"$(MODEL_ID)","hf_repo":"bartowski/gemma-2-2b-it-GGUF","hf_file":"gemma-2-2b-it-Q4_K_M.gguf","backend":"llamacpp","local_path":"llamacpp_models"}}' | jq .
	@echo "✓ gemma2-2b ready — send requests to port $(PORT) once download completes"

pull-gguf:
	@test -n "$(NODE_ID)"   || (echo "ERROR: NODE_ID not set"   && exit 1)
	@test -n "$(HF_REPO)"   || (echo "ERROR: HF_REPO not set"   && exit 1)
	@test -n "$(HF_FILE)"   || (echo "ERROR: HF_FILE not set"   && exit 1)
	@curl -s -X POST $(ADMIN_URL)/nodes/$(NODE_ID)/tasks \
	  -H 'Content-Type: application/json' \
	  -d '{"task_type":"PULL_MODEL","priority":80,"actor":"makefile","payload":{"model_id":"","hf_repo":"$(HF_REPO)","hf_file":"$(HF_FILE)","backend":"llamacpp","local_path":"llamacpp_models"}}' | jq .

runtime-status:
	@test -n "$(MODEL)"  || (echo "ERROR: MODEL not set" && exit 1)
	$(eval MODEL_ID := $(shell curl -s "$(ADMIN_URL)/models" | jq -r '.data[] | select(.name=="$(MODEL)") | .id'))
	@curl -s $(ADMIN_URL)/models/$(MODEL_ID)/runtime-status | jq '.runtimes[] | {hostname, state, container_id, last_used_at}'

clean:
	rm -rf $(BINARY_DIR)

# ─────────────────────────────────────────────────────────────────────────────
# Project management shortcuts
# ─────────────────────────────────────────────────────────────────────────────

# List all projects: make project-list
project-list:
	curl -s "$(ADMIN_URL)/projects" | jq '.data[] | {id, name, priority, status, runtime_count, reserved_vram_mb}'

# Create a project: make project-create ORG_ID=<id> TEAM_ID=<id> NAME="My Project" PRIORITY=NORMAL
project-create:
	@test -n "$(ORG_ID)"  || (echo "ERROR: ORG_ID not set"  && exit 1)
	@test -n "$(TEAM_ID)" || (echo "ERROR: TEAM_ID not set" && exit 1)
	@test -n "$(NAME)"    || (echo "ERROR: NAME not set"    && exit 1)
	curl -s -X POST $(ADMIN_URL)/projects \
	  -H 'Content-Type: application/json' \
	  -d '{"organization_id":"$(ORG_ID)","team_id":"$(TEAM_ID)","name":"$(NAME)","priority":"$(or $(PRIORITY),NORMAL)"}' | jq .

# Set project priority: make project-priority ID=<id> PRIORITY=CRITICAL
project-priority:
	@test -n "$(ID)"       || (echo "ERROR: ID not set"       && exit 1)
	@test -n "$(PRIORITY)" || (echo "ERROR: PRIORITY not set" && exit 1)
	curl -s -X POST $(ADMIN_URL)/projects/$(ID)/priority \
	  -H 'Content-Type: application/json' \
	  -d '{"priority":"$(PRIORITY)"}' | jq .

# Reserve VRAM for a project: make project-reserve ID=<id> VRAM_MB=81920
project-reserve:
	@test -n "$(ID)"      || (echo "ERROR: ID not set"      && exit 1)
	@test -n "$(VRAM_MB)" || (echo "ERROR: VRAM_MB not set" && exit 1)
	curl -s -X POST $(ADMIN_URL)/projects/$(ID)/reserve \
	  -H 'Content-Type: application/json' \
	  -d '{"reserved_vram_mb":$(VRAM_MB),"reserved_cpu_cores":$(or $(CPU),0),"reserved_memory_mb":$(or $(MEM_MB),0)}' | jq .

# Show project preemption history: make project-preemptions ID=<id>
project-preemptions:
	@test -n "$(ID)" || (echo "ERROR: ID not set" && exit 1)
	curl -s "$(ADMIN_URL)/projects/$(ID)/preemptions" | jq '.data[] | {id, preempted_priority, requesting_priority, trigger, created_at}'
