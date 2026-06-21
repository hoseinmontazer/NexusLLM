.PHONY: build build-gateway build-admin build-scheduler \
        run-gateway run-admin run-scheduler run-web web-install \
        test lint \
        docker-build docker-push \
        migrate dev-up dev-up-gpu dev-down \
        generate-key \
        placement-simulate node-status \
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
build: build-gateway build-admin build-scheduler

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

docker-push: docker-build
	docker push $(REGISTRY)/gateway:$(VERSION)
	docker push $(REGISTRY)/admin:$(VERSION)
	docker push $(REGISTRY)/scheduler:$(VERSION)

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

clean:
	rm -rf $(BINARY_DIR)
