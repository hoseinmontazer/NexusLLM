.PHONY: build build-gateway build-admin build-scheduler \
        run-gateway run-admin run-scheduler \
        test lint \
        docker-build docker-push \
        migrate dev-up dev-down \
        generate-key clean

# ─────────────────────────────────────────────────────────────────────────────
# Variables
# ─────────────────────────────────────────────────────────────────────────────
BINARY_DIR   := bin
REGISTRY     := registry.internal/nexusllm
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GO_FLAGS     := -ldflags="-w -s -X main.Version=$(VERSION)"
DB_DSN       ?= postgres://nexus:nexus@localhost:5432/nexusllm?sslmode=disable

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
# Run locally (requires postgres + redis from docker-compose)
# ─────────────────────────────────────────────────────────────────────────────
run-gateway: build-gateway
	NEXUS_SERVER_MODE=debug \
	NEXUS_DATABASE_DSN="$(DB_DSN)" \
	NEXUS_REDIS_ADDR="localhost:6379" \
	NEXUS_AUTH_JWTSECRET="dev-secret" \
	./$(BINARY_DIR)/nexus-gateway

run-admin: build-admin
	NEXUS_ADMIN_PORT=8081 \
	NEXUS_SERVER_MODE=debug \
	NEXUS_DATABASE_DSN="$(DB_DSN)" \
	NEXUS_REDIS_ADDR="localhost:6379" \
	./$(BINARY_DIR)/nexus-admin

run-scheduler: build-scheduler
	NEXUS_REDIS_ADDR="localhost:6379" \
	./$(BINARY_DIR)/nexus-scheduler

# ─────────────────────────────────────────────────────────────────────────────
# Test & Lint
# ─────────────────────────────────────────────────────────────────────────────
test:
	go test ./... -v -race -timeout 120s

lint:
	@which golangci-lint > /dev/null || (echo "install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest" && exit 1)
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
# ─────────────────────────────────────────────────────────────────────────────
migrate:
	@echo "→ Running all migrations..."
	psql "$(DB_DSN)" -f migrations/001_initial.sql
	psql "$(DB_DSN)" -f migrations/002_seed_data.sql
	psql "$(DB_DSN)" -f migrations/003_runtime_layer.sql
	psql "$(DB_DSN)" -f migrations/004_single_gpu_runtime_seed.sql
	psql "$(DB_DSN)" -f migrations/005_enterprise_platform.sql
	psql "$(DB_DSN)" -f migrations/006_controller_columns.sql
	@echo "✓ All migrations complete"

# ─────────────────────────────────────────────────────────────────────────────
# Local dev stack (Docker Compose — no GPU required)
# ─────────────────────────────────────────────────────────────────────────────
dev-up:
	@echo "→ Starting postgres + redis..."
	docker compose up -d postgres redis
	@echo "→ Waiting for postgres to be ready..."
	@sleep 4
	@echo "→ Running schema migration..."
	psql "$(DB_DSN)" -f migrations/001_initial.sql || true
	psql "$(DB_DSN)" -f migrations/002_seed_data.sql || true
	psql "$(DB_DSN)" -f migrations/003_runtime_layer.sql || true
	psql "$(DB_DSN)" -f migrations/004_single_gpu_runtime_seed.sql || true
	psql "$(DB_DSN)" -f migrations/005_enterprise_platform.sql || true
	psql "$(DB_DSN)" -f migrations/006_controller_columns.sql || true
	@echo "✓ Dev stack ready."
	@echo "  Run: make run-gateway    (port 8080)"
	@echo "  Run: make run-admin      (port 8081)"
	@echo "  Run: make run-scheduler"

# Full GPU stack (requires NVIDIA drivers + HF_TOKEN)
dev-up-gpu:
	@test -n "$(HF_TOKEN)" || (echo "ERROR: export HF_TOKEN=hf_..." && exit 1)
	docker compose -f docker-compose.single-gpu.yml up -d postgres redis
	@sleep 5
	psql "$(DB_DSN)" -f migrations/001_initial.sql || true
	psql "$(DB_DSN)" -f migrations/002_seed_data.sql || true
	psql "$(DB_DSN)" -f migrations/003_runtime_layer.sql || true
	psql "$(DB_DSN)" -f migrations/004_single_gpu_runtime_seed.sql || true
	psql "$(DB_DSN)" -f migrations/005_enterprise_platform.sql || true
	psql "$(DB_DSN)" -f migrations/006_controller_columns.sql || true
	docker compose -f docker-compose.single-gpu.yml up -d
	@echo "✓ Single-GPU stack started."
	@echo "  Gateway:    http://localhost:8080"
	@echo "  Admin API:  http://localhost:8081"
	@echo "  Prometheus: http://localhost:9100"
	@echo "  Grafana:    http://localhost:3000 (admin/admin)"

dev-down:
	docker compose down -v 2>/dev/null || true
	docker compose -f docker-compose.single-gpu.yml down -v 2>/dev/null || true

# ─────────────────────────────────────────────────────────────────────────────
# Utilities
# ─────────────────────────────────────────────────────────────────────────────
generate-key:
	@go run -v ./tools/genkey/main.go 2>/dev/null || \
	  (echo "Usage: go run ./tools/genkey/main.go")

clean:
	rm -rf $(BINARY_DIR)
