.PHONY: build build-gateway build-admin build-scheduler build-nodeagent build-web \
        run-gateway run-admin run-scheduler run-nodeagent run-web web-install \
        docker-build docker-push docker-build-web \
        test lint \
        migrate migrate-external migrate-dry \
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

# Helper: run a migration file inside the running postgres container
define run_migration
	docker compose exec -T postgres psql -U nexus -d nexusllm -f /migrations/$(1) 2>&1 | \
	  grep -vE "^(COMMIT|BEGIN|ALTER TABLE|CREATE INDEX|DO|INSERT 0|$$)" || true
endef

# ─────────────────────────────────────────────────────────────────────────────
# Build
# ─────────────────────────────────────────────────────────────────────────────
build: build-gateway build-admin build-scheduler build-nodeagent

build-gateway:
	@echo "→ Building nexus-gateway..."
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 go build $(GO_FLAGS) -o $(BINARY_DIR)/nexus-gateway ./cmd/gateway

build-admin:
	@echo "→ Building nexus-admin..."
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 go build $(GO_FLAGS) -o $(BINARY_DIR)/nexus-admin ./cmd/admin

build-scheduler:
	@echo "→ Building nexus-scheduler..."
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 go build $(GO_FLAGS) -o $(BINARY_DIR)/nexus-scheduler ./cmd/scheduler

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

run-scheduler: build-scheduler
	NEXUS_REDIS_ADDR="localhost:6379" \
	./$(BINARY_DIR)/nexus-scheduler

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
	docker build -f Dockerfile.gateway   -t $(REGISTRY)/gateway:$(VERSION)   .
	docker build -f Dockerfile.admin     -t $(REGISTRY)/admin:$(VERSION)     .
	docker build -f Dockerfile.scheduler -t $(REGISTRY)/scheduler:$(VERSION) .
	docker build -f Dockerfile.nodeagent -t $(REGISTRY)/nodeagent:$(VERSION) .
	docker build -f Dockerfile.web       -t $(REGISTRY)/web:$(VERSION)       .

docker-push: docker-build
	docker push $(REGISTRY)/gateway:$(VERSION)
	docker push $(REGISTRY)/admin:$(VERSION)
	docker push $(REGISTRY)/scheduler:$(VERSION)
	docker push $(REGISTRY)/nodeagent:$(VERSION)
	docker push $(REGISTRY)/web:$(VERSION)

# ─────────────────────────────────────────────────────────────────────────────
# Database migrations
# All migration files are idempotent — safe to re-run at any time.
# ─────────────────────────────────────────────────────────────────────────────
migrate:
	@echo "→ Waiting for postgres..."
	@until docker compose exec -T postgres pg_isready -U nexus -d nexusllm > /dev/null 2>&1; \
	  do echo "  waiting..."; sleep 2; done
	@echo "→ 001 initial schema"
	$(call run_migration,001_initial.sql)
	@echo "→ 002 seed data"
	$(call run_migration,002_seed_data.sql)
	@echo "→ 003 runtime layer"
	$(call run_migration,003_runtime_layer.sql)
	@echo "→ 004 single-GPU runtime seed"
	$(call run_migration,004_single_gpu_runtime_seed.sql)
	@echo "→ 005 AI platform schema"
	$(call run_migration,005_ai_platform.sql)
	@echo "→ 006 H200 platform seed"
	$(call run_migration,006_h200_platform_seed.sql)
	@echo "→ 007 agent tasks + runtimes"
	$(call run_migration,007_agent_tasks.sql)
	@echo "→ 008 node model cache"
	$(call run_migration,008_node_model_cache.sql)
	@echo "→ 009 resilience & lifecycle"
	$(call run_migration,009_resilience.sql)
	@echo "→ 010 lazy-load runtime manager"
	$(call run_migration,010_lazy_runtime.sql)
	@echo "→ 011 projects, preemption, deployment queue"
	$(call run_migration,011_projects.sql)
	@echo "→ 011b runtime config GPU"
	$(call run_migration,011_runtime_config_gpu.sql)
	@echo "→ 012 unified startup states"
	$(call run_migration,012_unified_startup_states.sql)
	@echo "→ 013 START_MODEL task type"
	$(call run_migration,013_start_model_task_type.sql)
	@echo "→ 014 execution mode"
	$(call run_migration,014_execution_mode.sql)
	@echo "→ 015 catchup schema"
	$(call run_migration,015_catchup_schema.sql)
	@echo "→ 016 workload policy"
	$(call run_migration,016_workload_policy.sql)
	@echo "→ 017 scheduler tables"
	$(call run_migration,017_scheduler.sql)
	@echo "→ 018 weighted priority"
	$(call run_migration,018_weighted_priority.sql)
	@echo "→ 019 HA replicas"
	$(call run_migration,019_ha_replicas.sql)
	@echo "→ 020 port allocator"
	$(call run_migration,020_port_allocator.sql)
	@echo "→ 021 missing columns"
	$(call run_migration,021_missing_columns.sql)
	@echo "→ 022 project API keys"
	$(call run_migration,022_project_api_keys.sql)
	@echo "→ 023 project policies & usage rollups"
	$(call run_migration,023_project_policies.sql)
	@echo "✓ All migrations complete"

# ─────────────────────────────────────────────────────────────────────────────
# External DB migrations
# Use these when postgres is NOT running in docker-compose (e.g. RDS, CloudSQL,
# managed Postgres, or a separate server).
#
# Required env var:
#   DB_DSN  — full Postgres connection string
#             postgres://user:pass@host:5432/nexusllm?sslmode=require
#
# Works even without local psql — uses Docker postgres image as psql client.
#
# Usage:
#   make migrate-external DB_DSN="postgres://nexus:secret@10.0.0.5:5432/nexusllm"
#   make migrate-external DB_DSN="postgres://nexus:nexus@192.168.0.200:5540/nexusllm"
#   make migrate-dry   # list files without connecting
# ─────────────────────────────────────────────────────────────────────────────
DB_DSN ?=

# Internal: run one SQL file against external DB using Docker psql client.
# Avoids requiring psql to be installed on the build machine.
define run_migration_external
	@echo "  → migrations/$(1)"
	@docker run --rm \
	  --network host \
	  -e PGPASSWORD="$$(echo $(DB_DSN) | sed 's|.*://[^:]*:\([^@]*\)@.*|\1|')" \
	  -v "$(CURDIR)/migrations:/migrations:ro" \
	  postgres:15-alpine \
	  psql "$(DB_DSN)" -f /migrations/$(1) -v ON_ERROR_STOP=1 \
	  2>&1 | grep -vE "^(COMMIT|BEGIN|ALTER|CREATE|DROP|INSERT 0|NOTICE|$$)" || true
endef

_check-dsn:
	@test -n "$(DB_DSN)" || { echo ""; echo "ERROR: DB_DSN is required."; echo ""; echo "  Usage: make migrate-external DB_DSN=\"postgres://user:pass@host:5432/db\""; echo ""; exit 1; }

migrate-external: _check-dsn
	@echo "→ Migrating external DB: $(DB_DSN)"
	@echo "  Using Docker postgres client (no local psql required)"
	@echo "  Testing connection..."
	@docker run --rm \
	  --network host \
	  postgres:15-alpine \
	  psql "$(DB_DSN)" -c "SELECT version();" -t 2>&1 | grep -q "PostgreSQL" \
	  || { echo "ERROR: Cannot connect to database. Check DB_DSN and network access."; exit 1; }
	@echo "  ✓ Connected"
	$(call run_migration_external,001_initial.sql)
	$(call run_migration_external,002_seed_data.sql)
	$(call run_migration_external,003_runtime_layer.sql)
	$(call run_migration_external,004_single_gpu_runtime_seed.sql)
	$(call run_migration_external,005_ai_platform.sql)
	$(call run_migration_external,005_enterprise_platform.sql)
	$(call run_migration_external,006_controller_columns.sql)
	$(call run_migration_external,006_h200_platform_seed.sql)
	$(call run_migration_external,007_agent_tasks.sql)
	$(call run_migration_external,008_node_model_cache.sql)
	$(call run_migration_external,009_resilience.sql)
	$(call run_migration_external,010_lazy_runtime.sql)
	$(call run_migration_external,011_projects.sql)
	$(call run_migration_external,011_runtime_config_gpu.sql)
	$(call run_migration_external,012_unified_startup_states.sql)
	$(call run_migration_external,013_start_model_task_type.sql)
	$(call run_migration_external,014_execution_mode.sql)
	$(call run_migration_external,015_catchup_schema.sql)
	$(call run_migration_external,016_workload_policy.sql)
	$(call run_migration_external,017_scheduler.sql)
	$(call run_migration_external,018_weighted_priority.sql)
	$(call run_migration_external,019_ha_replicas.sql)
	$(call run_migration_external,020_port_allocator.sql)
	$(call run_migration_external,021_missing_columns.sql)
	$(call run_migration_external,022_project_api_keys.sql)
	$(call run_migration_external,023_project_policies.sql)
	@echo "✓ All migrations complete on external DB"

# Dry-run: print the SQL files that would be applied without connecting
migrate-dry:
	@echo "→ Migrations that would be applied (dry-run):"
	@for f in $$(ls migrations/*.sql | sort); do echo "  $$f"; done
	@echo ""
	@echo "To run against an external DB:"
	@echo "  make migrate-external DB_DSN=\"postgres://user:pass@host:5432/nexusllm\""
	@echo ""
	@echo "To run a single migration manually:"
	@echo "  docker run --rm --network host -v \$$(pwd)/migrations:/m postgres:15-alpine \\"
	@echo "    psql \"\$$DB_DSN\" -f /m/023_project_policies.sql"

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
	@echo "NOTE: nexus-scheduler (GPU watcher) is only needed for vLLM endpoints."
	@echo "      All scheduling runs inside nexus-admin for llamacpp setups."
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
	  -d '{"model_name":"$(MODEL)","service_type":"CHAT","runtime_type":"GPU_RUNTIME","min_vram_mb":$(VRAM),"gpu_count":$(GPUS)}' | jq .

# Show node status
node-status:
	curl -s $(ADMIN_URL)/nodes | jq '.data[] | {hostname, status, total_cpu, total_ram_mb, total_vram_mb, last_heartbeat_at}'

# List all AI services
service-list:
	curl -s "$(ADMIN_URL)/services" | jq '.data[] | {name, service_type, runtime_type, endpoint_count, healthy_count}'

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
