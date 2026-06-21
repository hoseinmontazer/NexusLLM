// Package services implements the AI Service Registry — an extension of the
// existing model registry that adds first-class support for non-LLM service
// types (embeddings, rerankers, STT, TTS, OCR, agents, MCP servers).
//
// The registry is intentionally built on top of the existing models +
// model_endpoints tables so no existing code needs to change. The only
// additions are the service_type and runtime_type columns (added in migration 005).
package services

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
)

// ─────────────────────────────────────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────────────────────────────────────

// ServiceType mirrors placement.ServiceType but is reproduced here to keep the
// services package free of a dependency on placement.
type ServiceType = string

const (
	TypeChat      ServiceType = "CHAT"
	TypeEmbedding ServiceType = "EMBEDDING"
	TypeRerank    ServiceType = "RERANK"
	TypeSTT       ServiceType = "STT"
	TypeTTS       ServiceType = "TTS"
	TypeOCR       ServiceType = "OCR"
	TypeAgent     ServiceType = "AGENT"
	TypeMCP       ServiceType = "MCP"
)

// RuntimeType mirrors placement.RuntimeType.
type RuntimeType = string

const (
	RuntimeGPU RuntimeType = "GPU_RUNTIME"
	RuntimeCPU RuntimeType = "CPU_RUNTIME"
)

// ServiceRecord is the full view of a registered AI service.
type ServiceRecord struct {
	ID          string     `db:"id"           json:"id"`
	Name        string     `db:"name"         json:"name"`
	DisplayName string     `db:"display_name" json:"display_name"`
	ServiceType string     `db:"service_type" json:"service_type"`
	RuntimeType string     `db:"runtime_type" json:"runtime_type"`
	BackendType string     `db:"backend_type" json:"backend_type"`
	Provider    string     `db:"provider"     json:"provider"`
	MaxContext  int        `db:"max_context"  json:"max_context"`
	MaxOutput   int        `db:"max_output"   json:"max_output"`
	Enabled     bool       `db:"enabled"      json:"enabled"`
	Tags        []byte     `db:"tags"         json:"-"`
	TagsJSON    string     `json:"tags"`
	CreatedAt   time.Time  `db:"created_at"   json:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at"   json:"updated_at"`

	// Joined fields
	EndpointCount int `db:"endpoint_cnt" json:"endpoint_count"`
	HealthyCount  int `db:"healthy_cnt"  json:"healthy_count"`
}

// RegisterRequest is the input for registering a new AI service.
type RegisterRequest struct {
	Name        string   `json:"name"         binding:"required"`
	DisplayName string   `json:"display_name" binding:"required"`
	ServiceType string   `json:"service_type" binding:"required"`
	RuntimeType string   `json:"runtime_type"`
	BackendType string   `json:"backend_type"`
	Provider    string   `json:"provider"`
	MaxContext  int      `json:"max_context"`
	MaxOutput   int      `json:"max_output"`
	Host        string   `json:"host"         binding:"required"`
	Port        int      `json:"port"         binding:"required"`
	Tags        []string `json:"tags"`

	// Resource reservation
	MinVRAMMB   int64  `json:"min_vram_mb"`
	MaxVRAMMB   int64  `json:"max_vram_mb"`
	CPUCores    int    `json:"cpu_cores"`
	NUMANode    int    `json:"numa_node"`
	RAMMBLimit  int64  `json:"ram_mb"`
	Priority    string `json:"priority"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Registry
// ─────────────────────────────────────────────────────────────────────────────

// Registry manages the AI Service Registry.
type Registry struct {
	db  *sqlx.DB
	log *zap.Logger
}

// NewRegistry constructs an AI Service Registry.
func NewRegistry(db *sqlx.DB, log *zap.Logger) *Registry {
	return &Registry{db: db, log: log}
}

// Register inserts a new AI service into the registry.
// It creates the model row, a default version, and an endpoint row, and
// optionally persists a resource reservation.
// Returns the new model ID and endpoint ID.
func (r *Registry) Register(ctx context.Context, req RegisterRequest) (modelID, endpointID string, err error) {
	// Apply defaults
	if req.BackendType == "" {
		req.BackendType = defaultBackend(req.ServiceType, req.RuntimeType)
	}
	if req.RuntimeType == "" {
		req.RuntimeType = defaultRuntime(req.ServiceType)
	}
	if req.Provider == "" {
		req.Provider = "local"
	}
	if req.MaxContext == 0 {
		req.MaxContext = 4096
	}
	if req.MaxOutput == 0 {
		req.MaxOutput = 4096
	}
	if req.Priority == "" {
		req.Priority = "normal"
	}
	if req.NUMANode == 0 && req.CPUCores == 0 {
		req.NUMANode = -1
	}

	mID := uuid.New().String()
	host := req.Host
	if host == "" {
		host = "localhost"
	}

	// ── 1. Insert model row ────────────────────────────────────────────────
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO models
		  (id, name, display_name, provider, backend_type, service_type, runtime_type,
		   max_context, max_output, enabled, tags, vllm_endpoint)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,TRUE,$10,$11)`,
		mID, req.Name, req.DisplayName, req.Provider, req.BackendType,
		req.ServiceType, req.RuntimeType,
		req.MaxContext, req.MaxOutput,
		tagsJSON(req.Tags),
		fmt.Sprintf("http://%s:%d", host, req.Port),
	)
	if err != nil {
		return "", "", fmt.Errorf("register service: model insert: %w", err)
	}

	// ── 2. Default version ─────────────────────────────────────────────────
	_, _ = r.db.ExecContext(ctx,
		`INSERT INTO model_versions (id, model_id, version, is_default) VALUES ($1,$2,'v1',TRUE)`,
		uuid.New().String(), mID)

	// ── 3. Endpoint row ────────────────────────────────────────────────────
	epID := uuid.New().String()
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO model_endpoints
		  (id, model_id, host, port, base_path, weight, priority,
		   health_status, is_enabled, lifecycle_state, runtime_type)
		VALUES ($1,$2,$3,$4,'/v1',100,1,'unknown',TRUE,'active',$5)`,
		epID, mID, host, req.Port, req.RuntimeType,
	)
	if err != nil {
		_, _ = r.db.ExecContext(ctx, `DELETE FROM models WHERE id = $1`, mID)
		return "", "", fmt.Errorf("register service: endpoint insert: %w", err)
	}

	// ── 4. Resource reservation ────────────────────────────────────────────
	if req.MinVRAMMB > 0 || req.CPUCores > 0 || req.RAMMBLimit > 0 {
		_, _ = r.db.ExecContext(ctx, `
			INSERT INTO resource_reservations
			  (id, model_id, min_vram_mb, max_vram_mb, cpu_cores,
			   numa_node_pref, ram_mb, priority, preferred_runtime)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (model_id) DO UPDATE SET
			  min_vram_mb = EXCLUDED.min_vram_mb,
			  max_vram_mb = EXCLUDED.max_vram_mb,
			  cpu_cores   = EXCLUDED.cpu_cores,
			  numa_node_pref = EXCLUDED.numa_node_pref,
			  ram_mb      = EXCLUDED.ram_mb,
			  priority    = EXCLUDED.priority,
			  updated_at  = NOW()`,
			uuid.New().String(), mID,
			req.MinVRAMMB, req.MaxVRAMMB,
			req.CPUCores, req.NUMANode,
			req.RAMMBLimit, req.Priority, req.RuntimeType,
		)
	}

	r.log.Info("AI service registered",
		zap.String("name", req.Name),
		zap.String("service_type", req.ServiceType),
		zap.String("runtime_type", req.RuntimeType),
		zap.String("model_id", mID),
	)
	return mID, epID, nil
}

// List returns all registered AI services, optionally filtered by service_type.
func (r *Registry) List(ctx context.Context, serviceType string) ([]ServiceRecord, error) {
	q := `
		SELECT m.id, m.name, m.display_name, m.service_type, m.runtime_type,
		       m.backend_type, m.provider, m.max_context, m.max_output, m.enabled,
		       m.tags, m.created_at, m.updated_at,
		       COUNT(me.id)                                            AS endpoint_cnt,
		       COUNT(me.id) FILTER (WHERE me.health_status = 'healthy') AS healthy_cnt
		FROM models m
		LEFT JOIN model_endpoints me ON me.model_id = m.id AND me.is_enabled = TRUE`

	args := []interface{}{}
	if serviceType != "" {
		q += " WHERE m.service_type = $1"
		args = append(args, serviceType)
	}
	q += " GROUP BY m.id ORDER BY m.service_type, m.name"

	var rows []ServiceRecord
	if err := r.db.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, err
	}
	// Copy tags bytes to JSON string field for serialisation
	for i := range rows {
		if rows[i].Tags != nil {
			rows[i].TagsJSON = string(rows[i].Tags)
		} else {
			rows[i].TagsJSON = "[]"
		}
	}
	return rows, nil
}

// GetReservation returns the resource reservation for a model, if any.
func (r *Registry) GetReservation(ctx context.Context, modelID string) (*ResourceReservation, error) {
	var rr ResourceReservation
	err := r.db.GetContext(ctx, &rr, `
		SELECT id, model_id, min_vram_mb, max_vram_mb, cpu_cores,
		       numa_node_pref, ram_mb, priority, preferred_runtime,
		       created_at, updated_at
		FROM resource_reservations WHERE model_id = $1`, modelID)
	if err != nil {
		return nil, err
	}
	return &rr, nil
}

// UpsertReservation creates or updates a resource reservation.
func (r *Registry) UpsertReservation(ctx context.Context, req ReservationRequest) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO resource_reservations
		  (id, model_id, min_vram_mb, max_vram_mb, cpu_cores,
		   numa_node_pref, ram_mb, priority, preferred_runtime)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (model_id) DO UPDATE SET
		  min_vram_mb      = EXCLUDED.min_vram_mb,
		  max_vram_mb      = EXCLUDED.max_vram_mb,
		  cpu_cores        = EXCLUDED.cpu_cores,
		  numa_node_pref   = EXCLUDED.numa_node_pref,
		  ram_mb           = EXCLUDED.ram_mb,
		  priority         = EXCLUDED.priority,
		  preferred_runtime = EXCLUDED.preferred_runtime,
		  updated_at       = NOW()`,
		uuid.New().String(), req.ModelID,
		req.MinVRAMMB, req.MaxVRAMMB,
		req.CPUCores, req.NUMANodePref,
		req.RAMMB, req.Priority, req.PreferredRuntime,
	)
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// Resource Reservation types
// ─────────────────────────────────────────────────────────────────────────────

// ResourceReservation mirrors the resource_reservations table.
type ResourceReservation struct {
	ID               string    `db:"id"                json:"id"`
	ModelID          string    `db:"model_id"          json:"model_id"`
	MinVRAMMB        int64     `db:"min_vram_mb"       json:"min_vram_mb"`
	MaxVRAMMB        int64     `db:"max_vram_mb"       json:"max_vram_mb"`
	CPUCores         int       `db:"cpu_cores"         json:"cpu_cores"`
	NUMANodePref     int       `db:"numa_node_pref"    json:"numa_node_pref"`
	RAMMB            int64     `db:"ram_mb"            json:"ram_mb"`
	Priority         string    `db:"priority"          json:"priority"`
	PreferredRuntime string    `db:"preferred_runtime" json:"preferred_runtime"`
	CreatedAt        time.Time `db:"created_at"        json:"created_at"`
	UpdatedAt        time.Time `db:"updated_at"        json:"updated_at"`
}

// ReservationRequest is the input for creating/updating a resource reservation.
type ReservationRequest struct {
	ModelID          string `json:"model_id"          binding:"required"`
	MinVRAMMB        int64  `json:"min_vram_mb"`
	MaxVRAMMB        int64  `json:"max_vram_mb"`
	CPUCores         int    `json:"cpu_cores"`
	NUMANodePref     int    `json:"numa_node_pref"`
	RAMMB            int64  `json:"ram_mb"`
	Priority         string `json:"priority"`
	PreferredRuntime string `json:"preferred_runtime"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// defaultBackend returns the most appropriate backend_type for a service type.
func defaultBackend(serviceType, runtimeType string) string {
	if runtimeType == RuntimeCPU {
		return "cpu_native"
	}
	switch serviceType {
	case TypeChat:
		return "vllm"
	case TypeEmbedding:
		return "openai_compat" // TEI, Infinity, FastEmbed
	case TypeRerank:
		return "openai_compat" // TEI rerank, Cohere-compat
	case TypeSTT:
		return "openai_compat" // faster-whisper HTTP, whisper.cpp server
	case TypeTTS:
		return "openai_compat" // Coqui TTS, Kokoro TTS
	case TypeOCR:
		return "openai_compat"
	case TypeAgent, TypeMCP:
		return "openai_compat"
	default:
		return "openai_compat"
	}
}

// defaultRuntime returns the most appropriate runtime_type for a service type.
func defaultRuntime(serviceType string) string {
	switch serviceType {
	case TypeChat:
		return RuntimeGPU
	case TypeEmbedding, TypeRerank, TypeSTT, TypeTTS, TypeOCR, TypeAgent, TypeMCP:
		return RuntimeCPU
	default:
		return RuntimeGPU
	}
}

func tagsJSON(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	b := "["
	for i, t := range tags {
		if i > 0 {
			b += ","
		}
		b += `"` + t + `"`
	}
	return b + "]"
}
