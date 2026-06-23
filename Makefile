.PHONY: build build-gateway build-admin build-scheduler build-nodeagent \
        run-gateway run-admin run-scheduler run-nodeagent run-web web-install \
        test lint \
        docker-build docker-push \
        migrate dev-up dev-up-gpu dev-down \
        generate-key \
        placement-simulate node-status \
        deploy-gemma2-2b pull-gguf runtime-status \
        project-list project-create project-priority project-reserve project-preemptions \
        clean

BINARY_DIR := bin
REGISTRY   := registry.internal/nexusllm
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

docker-push: docker-build
	docker push $(REGISTRY)/gateway:$(VERSION)
	docker push $(REGISTRY)/admin:$(VERSION)
	docker push $(REGISTRY)/scheduler:$(VERSION)
	docker push $(REGISTRY)/nodeagent:$(VERSION)

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
	@echo "✓ All migrations complete"

# ─────────────────────────────────────────────────────────────────────────────
# Local dev stack
# ─────────────────────────────────────────────────────────────────────────────
dev-up:
	@echo "→ Starting postgres + redis..."
	docker compose up -d postgres redis
	$(MAKE) migrate
	@echo ""
	@echo "✓ Dev stack ready — run services:"
	@echo "  make run-gateway    →  http://localhost:8080"
	@echo "  make run-admin      →  http://localhost:8081"
	@echo "  make run-scheduler"
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
