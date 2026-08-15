package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/nexusllm/nexusllm/internal/catalog"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"go.uber.org/zap"
)

// CatalogHandler exposes the Provider Catalog admin REST API.
type CatalogHandler struct {
	db        *sqlx.DB
	store     *catalog.ProviderStore
	rules     *catalog.RuleStore
	scheduler *catalog.SyncScheduler
	resolver  *catalog.VirtualModelResolver
	registry  *runtime.Registry
	log       *zap.Logger
	engine    *policy.Engine // required for permission-restore Redis sync — see WithPolicyEngine
}

// NewCatalogHandler constructs a CatalogHandler.
func NewCatalogHandler(
	db *sqlx.DB,
	scheduler *catalog.SyncScheduler,
	resolver *catalog.VirtualModelResolver,
	registry *runtime.Registry,
) *CatalogHandler {
	log, _ := zap.NewProduction()
	return &CatalogHandler{
		db:        db,
		store:     catalog.NewProviderStore(db),
		rules:     catalog.NewRuleStore(db),
		scheduler: scheduler,
		resolver:  resolver,
		registry:  registry,
		log:       log,
	}
}

// WithPolicyEngine attaches the gateway's policy engine so permission
// restoration can synchronize restored team_model_permissions rows into the
// live Redis ACL set via the canonical SetModelAllowed path. See
// RuntimeHandler.WithPolicyEngine / restorePermissionsFromSnapshot.
func (h *CatalogHandler) WithPolicyEngine(e *policy.Engine) *CatalogHandler {
	h.engine = e
	return h
}

// ── Provider CRUD ─────────────────────────────────────────────────────────────

// ListProviders handles GET /admin/v1/providers
func (h *CatalogHandler) ListProviders(c *gin.Context) {
	providers, err := h.store.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Strip api_key from list response — only indicate presence.
	type safeProvider struct {
		ID                  string     `json:"id"`
		Name                string     `json:"name"`
		DisplayName         string     `json:"display_name"`
		BackendType         string     `json:"backend_type"`
		BaseURL             string     `json:"base_url"`
		APIKeySet           bool       `json:"api_key_set"`
		ExposureMode        string     `json:"exposure_mode"`
		CatalogSyncEnabled  bool       `json:"catalog_sync_enabled"`
		CatalogSyncInterval int        `json:"catalog_sync_interval"`
		CatalogDirectExpose bool       `json:"catalog_direct_expose"`
		CatalogExposePrefix string     `json:"catalog_expose_prefix"`
		CatalogLastSyncedAt *time.Time `json:"catalog_last_synced_at"`
		CatalogModelCount   int        `json:"catalog_model_count"`
		CatalogSyncStatus   string     `json:"catalog_sync_status"`
		CatalogSyncError    string     `json:"catalog_sync_error,omitempty"`
		ProxyURL            string     `json:"proxy_url,omitempty"`
		Enabled             bool       `json:"enabled"`
		Health              string     `json:"health"`
		LastHealthCheck     *time.Time `json:"last_health_check"`
		CreatedAt           time.Time  `json:"created_at"`
		UpdatedAt           time.Time  `json:"updated_at"`
	}
	out := make([]safeProvider, len(providers))
	for i, p := range providers {
		em := string(p.ExposureMode)
		if em == "" {
			em = "managed"
		}
		out[i] = safeProvider{
			ID: p.ID, Name: p.Name, DisplayName: p.DisplayName,
			BackendType: p.BackendType, BaseURL: p.BaseURL,
			APIKeySet:           p.APIKey != "",
			ExposureMode:        em,
			CatalogSyncEnabled:  p.CatalogSyncEnabled,
			CatalogSyncInterval: p.CatalogSyncInterval,
			CatalogDirectExpose: p.CatalogDirectExpose,
			CatalogExposePrefix: p.CatalogExposePrefix,
			CatalogLastSyncedAt: p.CatalogLastSyncedAt,
			CatalogModelCount:   p.CatalogModelCount,
			CatalogSyncStatus:   p.CatalogSyncStatus,
			CatalogSyncError:    p.CatalogSyncError,
			ProxyURL:            p.ProxyURL,
			Enabled:             p.Enabled,
			Health:              p.Health,
			LastHealthCheck:     p.LastHealthCheck,
			CreatedAt:           p.CreatedAt,
			UpdatedAt:           p.UpdatedAt,
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "total": len(out)})
}

// GetProvider handles GET /admin/v1/providers/:id
func (h *CatalogHandler) GetProvider(c *gin.Context) {
	id := c.Param("id")
	p, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "provider not found: " + id})
		return
	}
	// Return the same snake_case shape as ListProviders — never the raw struct,
	// which serialises Go field names as PascalCase and omits the api_key safely.
	em := string(p.ExposureMode)
	if em == "" {
		em = "managed"
	}
	c.JSON(http.StatusOK, gin.H{
		"id":                     p.ID,
		"name":                   p.Name,
		"display_name":           p.DisplayName,
		"backend_type":           p.BackendType,
		"base_url":               p.BaseURL,
		"api_key_set":            p.APIKey != "",
		"exposure_mode":          em,
		"catalog_sync_enabled":   p.CatalogSyncEnabled,
		"catalog_sync_interval":  p.CatalogSyncInterval,
		"catalog_direct_expose":  p.CatalogDirectExpose,
		"catalog_expose_prefix":  p.CatalogExposePrefix,
		"catalog_last_synced_at": p.CatalogLastSyncedAt,
		"catalog_model_count":    p.CatalogModelCount,
		"catalog_sync_status":    p.CatalogSyncStatus,
		"catalog_sync_error":     p.CatalogSyncError,
		"proxy_url":              p.ProxyURL,
		"enabled":                p.Enabled,
		"health":                 p.Health,
		"last_health_check":      p.LastHealthCheck,
		"created_at":             p.CreatedAt,
		"updated_at":             p.UpdatedAt,
	})
}

// CreateProvider handles POST /admin/v1/providers
func (h *CatalogHandler) CreateProvider(c *gin.Context) {
	var in struct {
		Name                string `json:"name"         binding:"required"`
		DisplayName         string `json:"display_name" binding:"required"`
		BackendType         string `json:"backend_type" binding:"required"`
		BaseURL             string `json:"base_url"     binding:"required"`
		APIKey              string `json:"api_key"`
		APIKeyHeader        string `json:"api_key_header"`
		ExposureMode        string `json:"exposure_mode"`
		CatalogSyncEnabled  bool   `json:"catalog_sync_enabled"`
		CatalogSyncInterval int    `json:"catalog_sync_interval"`
		CatalogDirectExpose bool   `json:"catalog_direct_expose"`
		CatalogExposePrefix string `json:"catalog_expose_prefix"`
		ProxyURL            string `json:"proxy_url"`
		RequestTimeoutSec   int    `json:"request_timeout_seconds"`
		MaxRetries          int    `json:"max_retries"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.ProxyURL != "" {
		if err := runtime.ValidateProxyURL(in.ProxyURL); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if in.APIKeyHeader == "" {
		in.APIKeyHeader = "Authorization"
	}
	if in.CatalogSyncInterval <= 0 {
		in.CatalogSyncInterval = 3600
	}
	if in.RequestTimeoutSec <= 0 {
		in.RequestTimeoutSec = 120
	}
	if in.MaxRetries <= 0 {
		in.MaxRetries = 2
	}
	// Validate and default exposure_mode.
	switch in.ExposureMode {
	case "managed", "catalog", "hybrid":
		// valid
	case "":
		in.ExposureMode = "managed"
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "exposure_mode must be one of: managed, catalog, hybrid"})
		return
	}
	id := uuid.New().String()
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO providers
		  (id, name, display_name, backend_type, base_url, api_key, api_key_header,
		   exposure_mode,
		   catalog_sync_enabled, catalog_sync_interval, catalog_direct_expose,
		   catalog_expose_prefix, proxy_url, request_timeout_seconds, max_retries)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,''),$14,$15)`,
		id, in.Name, in.DisplayName, in.BackendType, in.BaseURL,
		in.APIKey, in.APIKeyHeader,
		in.ExposureMode,
		in.CatalogSyncEnabled, in.CatalogSyncInterval,
		in.ExposureMode == "catalog" || in.ExposureMode == "hybrid",
		in.CatalogExposePrefix, in.ProxyURL, in.RequestTimeoutSec, in.MaxRetries,
	)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "provider name already exists or DB error: " + err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id, "name": in.Name, "exposure_mode": in.ExposureMode, "status": "created"})
}

// UpdateProvider handles PUT /admin/v1/providers/:id
func (h *CatalogHandler) UpdateProvider(c *gin.Context) {
	id := c.Param("id")
	var in struct {
		DisplayName         *string `json:"display_name"`
		BaseURL             *string `json:"base_url"`
		APIKey              *string `json:"api_key"`
		ExposureMode        *string `json:"exposure_mode"`
		CatalogSyncEnabled  *bool   `json:"catalog_sync_enabled"`
		CatalogSyncInterval *int    `json:"catalog_sync_interval"`
		CatalogDirectExpose *bool   `json:"catalog_direct_expose"`
		CatalogExposePrefix *string `json:"catalog_expose_prefix"`
		ProxyURL            *string `json:"proxy_url"`
		Enabled             *bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.ProxyURL != nil {
		if err := runtime.ValidateProxyURL(*in.ProxyURL); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	ctx := c.Request.Context()
	if in.DisplayName != nil {
		_, _ = h.db.ExecContext(ctx, `UPDATE providers SET display_name=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.DisplayName)
	}
	if in.BaseURL != nil {
		// FIX C-4: return error if safety-critical fields fail to persist.
		if _, err := h.db.ExecContext(ctx, `UPDATE providers SET base_url=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.BaseURL); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update base_url: " + err.Error()})
			return
		}
	}
	if in.APIKey != nil {
		// FIX C-4: return error if API key fails to persist — silent failure here
		// means the old (possibly invalid) credential continues to be used.
		if _, err := h.db.ExecContext(ctx, `UPDATE providers SET api_key=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.APIKey); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update api_key: " + err.Error()})
			return
		}
	}
	if in.ExposureMode != nil {
		switch *in.ExposureMode {
		case "managed", "catalog", "hybrid":
			// valid — the DB trigger will update catalog_direct_expose automatically.
			if err := h.store.UpdateExposureMode(ctx, id, catalog.ExposureMode(*in.ExposureMode)); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update exposure_mode: " + err.Error()})
				return
			}
			if h.resolver != nil {
				h.resolver.Invalidate()
			}
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "exposure_mode must be one of: managed, catalog, hybrid"})
			return
		}
	}
	if in.CatalogSyncEnabled != nil {
		_, _ = h.db.ExecContext(ctx, `UPDATE providers SET catalog_sync_enabled=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.CatalogSyncEnabled)
	}
	if in.CatalogSyncInterval != nil {
		_, _ = h.db.ExecContext(ctx, `UPDATE providers SET catalog_sync_interval=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.CatalogSyncInterval)
	}
	if in.CatalogDirectExpose != nil {
		_, _ = h.db.ExecContext(ctx, `UPDATE providers SET catalog_direct_expose=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.CatalogDirectExpose)
		if h.resolver != nil {
			h.resolver.Invalidate()
		}
	}
	if in.CatalogExposePrefix != nil {
		_, _ = h.db.ExecContext(ctx, `UPDATE providers SET catalog_expose_prefix=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.CatalogExposePrefix)
		if h.resolver != nil {
			h.resolver.Invalidate()
		}
	}
	if in.ProxyURL != nil {
		_, _ = h.db.ExecContext(ctx, `UPDATE providers SET proxy_url=NULLIF($2,''),updated_at=NOW() WHERE id::text=$1`, id, *in.ProxyURL)
	}
	if in.Enabled != nil {
		// FIX C-4: return error if enable/disable fails to persist.
		if _, err := h.db.ExecContext(ctx, `UPDATE providers SET enabled=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.Enabled); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update enabled: " + err.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"message": "provider updated", "id": id})
}

// DeleteProvider handles DELETE /admin/v1/providers/:id
func (h *CatalogHandler) DeleteProvider(c *gin.Context) {
	id := c.Param("id")
	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE providers SET enabled=FALSE,updated_at=NOW() WHERE id::text=$1`, id)
	if h.resolver != nil {
		h.resolver.Invalidate()
	}
	c.JSON(http.StatusOK, gin.H{"message": "provider disabled", "id": id})
}

// SyncProvider handles POST /admin/v1/providers/:id/sync
func (h *CatalogHandler) SyncProvider(c *gin.Context) {
	id := c.Param("id")
	// Use context.Background() — the request context is cancelled as soon as
	// the 202 response is sent, which would abort the sync goroutine immediately.
	go func() {
		if h.scheduler != nil {
			_ = h.scheduler.TriggerSync(context.Background(), id)
			if h.resolver != nil {
				h.resolver.Invalidate()
			}
		}
	}()
	c.JSON(http.StatusAccepted, gin.H{"message": "sync triggered", "provider_id": id})
}

// HealthCheck handles GET /admin/v1/providers/:id/health
func (h *CatalogHandler) HealthCheck(c *gin.Context) {
	id := c.Param("id")
	p, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "provider not found"})
		return
	}
	client, err := runtime.BuildProviderClient(p.Transport())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	bt := runtime.BackendType(p.BackendType)
	factory := runtime.NewFactory(client)
	backend, _ := factory.Build(bt)
	if backend == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown backend_type: " + p.BackendType})
		return
	}
	health := backend.Health(c.Request.Context(), p.BaseURL, client)
	status := "healthy"
	if health.Status != runtime.StatusHealthy {
		status = string(health.Status)
	}
	_ = h.store.UpdateHealth(c.Request.Context(), id, status)
	c.JSON(http.StatusOK, gin.H{
		"provider_id": id,
		"health":      status,
		"latency_ms":  health.LatencyMs,
		"error":       health.Error,
	})
}

// UpdateTransport handles PUT /admin/v1/providers/:id/transport
func (h *CatalogHandler) UpdateTransport(c *gin.Context) {
	id := c.Param("id")
	var in struct {
		ProxyURL                     *string `json:"proxy_url"`
		TLSInsecureSkipVerify        *bool   `json:"tls_insecure_skip_verify"`
		ConnectTimeoutSeconds        *int    `json:"connect_timeout_seconds"`
		ReadTimeoutSeconds           *int    `json:"read_timeout_seconds"`
		IdleConnTimeoutSeconds       *int    `json:"idle_conn_timeout_seconds"`
		ResponseHeaderTimeoutSeconds *int    `json:"response_header_timeout_seconds"`
		MaxIdleConnsPerHost          *int    `json:"max_idle_conns_per_host"`
		MaxConnsPerHost              *int    `json:"max_conns_per_host"`
		DisableHTTP2                 *bool   `json:"disable_http2"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.ProxyURL != nil {
		if err := runtime.ValidateProxyURL(*in.ProxyURL); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	ctx := c.Request.Context()
	if in.ProxyURL != nil {
		_, _ = h.db.ExecContext(ctx, `UPDATE providers SET proxy_url=NULLIF($2,''),updated_at=NOW() WHERE id::text=$1`, id, *in.ProxyURL)
	}
	if in.TLSInsecureSkipVerify != nil {
		_, _ = h.db.ExecContext(ctx, `UPDATE providers SET tls_insecure_skip_verify=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.TLSInsecureSkipVerify)
	}
	if in.ConnectTimeoutSeconds != nil {
		_, _ = h.db.ExecContext(ctx, `UPDATE providers SET connect_timeout_seconds=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.ConnectTimeoutSeconds)
	}
	if in.ReadTimeoutSeconds != nil {
		_, _ = h.db.ExecContext(ctx, `UPDATE providers SET read_timeout_seconds=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.ReadTimeoutSeconds)
	}
	if in.IdleConnTimeoutSeconds != nil {
		_, _ = h.db.ExecContext(ctx, `UPDATE providers SET idle_conn_timeout_seconds=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.IdleConnTimeoutSeconds)
	}
	if in.ResponseHeaderTimeoutSeconds != nil {
		_, _ = h.db.ExecContext(ctx, `UPDATE providers SET response_header_timeout_seconds=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.ResponseHeaderTimeoutSeconds)
	}
	if in.MaxIdleConnsPerHost != nil {
		_, _ = h.db.ExecContext(ctx, `UPDATE providers SET max_idle_conns_per_host=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.MaxIdleConnsPerHost)
	}
	if in.MaxConnsPerHost != nil {
		_, _ = h.db.ExecContext(ctx, `UPDATE providers SET max_conns_per_host=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.MaxConnsPerHost)
	}
	if in.DisableHTTP2 != nil {
		_, _ = h.db.ExecContext(ctx, `UPDATE providers SET disable_http2=$2,updated_at=NOW() WHERE id::text=$1`, id, *in.DisableHTTP2)
	}
	if h.resolver != nil {
		h.resolver.Invalidate()
	}
	c.JSON(http.StatusOK, gin.H{"message": "transport updated", "provider_id": id})
}

// ── Catalog ───────────────────────────────────────────────────────────────────

// ListCatalog handles GET /admin/v1/providers/:id/catalog
func (h *CatalogHandler) ListCatalog(c *gin.Context) {
	id := c.Param("id")
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	perPage, _ := strconv.Atoi(c.DefaultQuery("per_page", "50"))
	if page < 1 {
		page = 1
	}
	if perPage < 1 || perPage > 200 {
		perPage = 50
	}
	offset := (page - 1) * perPage

	q := "%" + c.Query("q") + "%"
	capability := c.Query("capability")
	tag := c.Query("tag")
	exposed := c.Query("exposed")

	type row struct {
		ID                string    `db:"id"               json:"id"`
		ProviderModelID   string    `db:"provider_model_id" json:"provider_model_id"`
		DisplayName       string    `db:"display_name"     json:"display_name"`
		ContextLength     *int      `db:"context_length"   json:"context_length"`
		InputCost         *float64  `db:"input_cost_per_1m" json:"input_cost_per_1m"`
		OutputCost        *float64  `db:"output_cost_per_1m" json:"output_cost_per_1m"`
		SupportsStreaming bool      `db:"supports_streaming" json:"supports_streaming"`
		SupportsTools     bool      `db:"supports_tools"   json:"supports_tools"`
		SupportsVision    bool      `db:"supports_vision"  json:"supports_vision"`
		SupportsAudio     bool      `db:"supports_audio"   json:"supports_audio"`
		SupportsEmbed     bool      `db:"supports_embeddings" json:"supports_embeddings"`
		SupportsReason    bool      `db:"supports_reasoning" json:"supports_reasoning"`
		TagsRaw           string    `db:"tags_raw"         json:"-"`
		Tags              []string  `db:"-"                json:"tags"`
		Enabled           bool      `db:"enabled"          json:"enabled"`
		LastSeenAt        time.Time `db:"last_seen_at"    json:"last_seen_at"`
	}

	base := `FROM provider_remote_models
		WHERE provider_id::text=$1
		  AND (provider_model_id ILIKE $2 OR display_name ILIKE $2)`
	args := []interface{}{id, q}
	argN := 3

	if capability != "" {
		switch capability {
		case "tools":
			base += ` AND supports_tools=TRUE`
		case "vision":
			base += ` AND supports_vision=TRUE`
		case "audio":
			base += ` AND supports_audio=TRUE`
		case "embedding":
			base += ` AND supports_embeddings=TRUE`
		case "reasoning":
			base += ` AND supports_reasoning=TRUE`
		}
	}
	if tag != "" {
		base += ` AND $` + strconv.Itoa(argN) + `=ANY(tags)`
		args = append(args, tag)
		argN++
	}
	if exposed == "true" {
		base += ` AND enabled=TRUE`
	} else if exposed == "false" {
		base += ` AND enabled=FALSE`
	}

	var total int
	_ = h.db.QueryRowContext(c.Request.Context(), `SELECT COUNT(*) `+base, args...).Scan(&total)

	args = append(args, perPage, offset)
	var rows []row
	_ = h.db.SelectContext(c.Request.Context(), &rows,
		`SELECT id::text, provider_model_id, display_name, context_length,
		        input_cost_per_1m, output_cost_per_1m,
		        supports_streaming, supports_tools, supports_vision,
		        supports_audio, supports_embeddings, supports_reasoning,
		        COALESCE(array_to_string(tags,','),'') AS tags_raw,
		        enabled, last_seen_at `+base+
			` ORDER BY provider_model_id LIMIT $`+strconv.Itoa(argN)+` OFFSET $`+strconv.Itoa(argN+1),
		args...)
	for i := range rows {
		if rows[i].TagsRaw != "" {
			for _, t := range splitComma(rows[i].TagsRaw) {
				rows[i].Tags = append(rows[i].Tags, t)
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": total, "page": page, "per_page": perPage})
}

func splitComma(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// ── Catalog Alias (Mode A public model) ──────────────────────────────────────

// RegisterCatalogAlias handles POST /admin/v1/models/catalog-alias
// Creates a NexusLLM Public Model backed by a catalog entry.
func (h *CatalogHandler) RegisterCatalogAlias(c *gin.Context) {
	var in struct {
		Name            string   `json:"name"         binding:"required"`
		DisplayName     string   `json:"display_name"`
		ProviderID      string   `json:"provider_id"  binding:"required"`
		ProviderModelID string   `json:"provider_model_id" binding:"required"`
		ServiceType     string   `json:"service_type"`
		Capabilities    []string `json:"capabilities"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.ServiceType == "" {
		in.ServiceType = "CHAT"
	}
	if in.DisplayName == "" {
		in.DisplayName = in.Name
	}

	// Load provider.
	p, err := h.store.Get(c.Request.Context(), in.ProviderID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider not found: " + in.ProviderID})
		return
	}

	// Look up catalog entry.
	var catalogID, displayName string
	err = h.db.QueryRowContext(c.Request.Context(),
		`SELECT id::text, display_name FROM provider_remote_models
		 WHERE provider_id::text=$1 AND provider_model_id=$2 LIMIT 1`,
		p.ID, in.ProviderModelID,
	).Scan(&catalogID, &displayName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "catalog entry not found: " + in.ProviderModelID})
		return
	}
	if in.DisplayName == in.Name && displayName != "" {
		in.DisplayName = displayName
	}

	caps := in.Capabilities
	if len(caps) == 0 {
		caps = []string{"chat", "completion"}
	}
	capsJSON, _ := marshalJSON(caps)

	mID := uuid.New().String()
	_, err = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO models
		  (id, name, display_name, provider, backend_type, service_type,
		   max_context, max_output, enabled, tags, capabilities,
		   provider_name, provider_is_external, provider_id, provider_catalog_id)
		VALUES ($1,$2,$3,$4,$5,$6,128000,16384,TRUE,'[]'::jsonb,$7::jsonb,
		        $4,TRUE,$8::uuid,$9::uuid)`,
		mID, in.Name, in.DisplayName,
		p.BackendType, p.BackendType, in.ServiceType,
		capsJSON, p.ID, catalogID,
	)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "model name already exists or DB error: " + err.Error()})
		return
	}
	_, _ = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO model_versions (id,model_id,version,is_default) VALUES ($1,$2,'v1',TRUE)`,
		uuid.New().String(), mID)

	// Restore team_model_permissions from snapshot if this model name was previously
	// soft-deleted.  No-op on first-ever registration.
	if _, restoreErr := restorePermissionsFromSnapshot(c.Request.Context(), h.db, h.engine, h.log, in.Name, mID); restoreErr != nil {
		h.log.Error("permission restore failed — this registration will NOT have any team grants "+
			"automatically recovered from a previous deletion of this model name; grant access manually if needed",
			zap.String("model_name", in.Name), zap.String("model_id", mID), zap.Error(restoreErr))
	}

	epID := uuid.New().String()
	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO model_endpoints
		  (id, model_id, host, port, base_path, weight, priority,
		   health_status, is_enabled, lifecycle_state,
		   upstream_api_key, upstream_base_url, upstream_proxy, upstream_model_name,
		   provider_proxy_url)
		VALUES ($1,$2,'0.0.0.0',0,'/v1',100,1,'unknown',TRUE,'active',
		        NULLIF($3,''), NULLIF($4,''), '', NULLIF($5,''),
		        NULLIF($6,''))`,
		epID, mID, p.APIKey, p.BaseURL, in.ProviderModelID, p.ProxyURL,
	)

	if h.registry != nil {
		_ = h.registry.Reload(c.Request.Context())
	}
	c.JSON(http.StatusCreated, gin.H{
		"model_id":          mID,
		"model_name":        in.Name,
		"endpoint_id":       epID,
		"provider_id":       p.ID,
		"provider_model_id": in.ProviderModelID,
		"status":            "active",
	})
}

func marshalJSON(v interface{}) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]", err
	}
	return string(b), nil
}

func splitStr(s string, sep rune) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == sep })
}

// ── Exposure Rules ────────────────────────────────────────────────────────────

// ListExposedModelIDs handles GET /admin/v1/providers/:id/exposed-models
// Returns the set of provider_model_id values that have an allow_model rule.
// Used by the catalog UI to show which models are currently toggled on.
func (h *CatalogHandler) ListExposedModelIDs(c *gin.Context) {
	id := c.Param("id")
	type row struct {
		ModelID string `db:"model_id"`
		RuleID  string `db:"id"`
	}
	var rows []row
	err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id::text, COALESCE(model_id,'') AS model_id
		FROM provider_exposure_rules
		WHERE provider_id::text=$1 AND rule_type='allow_model' AND enabled=TRUE`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Return map of model_id → rule_id so UI can delete individual rules.
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.ModelID != "" {
			m[r.ModelID] = r.RuleID
		}
	}
	c.JSON(http.StatusOK, gin.H{"exposed": m, "count": len(m)})
}

// BulkExposeModels handles POST /admin/v1/providers/:id/expose-models
// Creates allow_model rules for the given model IDs and enables direct exposure.
func (h *CatalogHandler) BulkExposeModels(c *gin.Context) {
	id := c.Param("id")
	var in struct {
		ModelIDs []string `json:"model_ids" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(in.ModelIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model_ids must not be empty"})
		return
	}

	ctx := c.Request.Context()
	created := 0
	for _, modelID := range in.ModelIDs {
		if modelID == "" {
			continue
		}
		rid := uuid.New().String()
		_, err := h.db.ExecContext(ctx, `
			INSERT INTO provider_exposure_rules
			  (id, provider_id, rule_type, model_id, priority)
			VALUES ($1, $2::uuid, 'allow_model', $3, 50)
			ON CONFLICT DO NOTHING`,
			rid, id, modelID,
		)
		if err == nil {
			created++
		}
	}

	// Auto-enable direct catalog expose so virtual models are immediately routable.
	_, _ = h.db.ExecContext(ctx,
		`UPDATE providers SET catalog_direct_expose=TRUE, updated_at=NOW() WHERE id::text=$1`, id)

	if h.resolver != nil {
		h.resolver.Invalidate()
	}
	c.JSON(http.StatusOK, gin.H{
		"created":     created,
		"provider_id": id,
		"note":        "catalog_direct_expose enabled — selected models are now routable as virtual endpoints",
	})
}

// BulkHideModels handles POST /admin/v1/providers/:id/hide-models
// Deletes allow_model rules for the given model IDs.
func (h *CatalogHandler) BulkHideModels(c *gin.Context) {
	id := c.Param("id")
	var in struct {
		RuleIDs []string `json:"rule_ids" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	for _, rid := range in.RuleIDs {
		_, _ = h.db.ExecContext(ctx,
			`DELETE FROM provider_exposure_rules WHERE id::text=$1 AND provider_id::text=$2`, rid, id)
	}
	if h.resolver != nil {
		h.resolver.Invalidate()
	}
	c.JSON(http.StatusOK, gin.H{"hidden": len(in.RuleIDs), "provider_id": id})
}

// ListRules handles GET /admin/v1/providers/:id/rules
func (h *CatalogHandler) ListRules(c *gin.Context) {
	id := c.Param("id")
	rules, err := h.rules.ListAll(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rules, "total": len(rules)})
}

// CreateRule handles POST /admin/v1/providers/:id/rules
func (h *CatalogHandler) CreateRule(c *gin.Context) {
	providerID := c.Param("id")
	var in struct {
		RuleType          string   `json:"rule_type" binding:"required"`
		Pattern           string   `json:"pattern"`
		ModelID           string   `json:"model_id"`
		RequireStreaming  *bool    `json:"require_streaming"`
		RequireTools      *bool    `json:"require_tools"`
		RequireVision     *bool    `json:"require_vision"`
		RequireAudio      *bool    `json:"require_audio"`
		RequireEmbeddings *bool    `json:"require_embeddings"`
		RequireReasoning  *bool    `json:"require_reasoning"`
		DenyTags          []string `json:"deny_tags"`
		Priority          int      `json:"priority"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.Priority == 0 {
		in.Priority = 100
	}
	rid := uuid.New().String()
	// Use pq.Array for deny_tags — safe for all characters including
	// slashes and hyphens that appear in provider tag values.
	denyTagsArray := pq.Array(in.DenyTags)
	if len(in.DenyTags) == 0 {
		denyTagsArray = pq.Array([]string{})
	}
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO provider_exposure_rules
		  (id,provider_id,rule_type,pattern,model_id,
		   require_streaming,require_tools,require_vision,
		   require_audio,require_embeddings,require_reasoning,
		   deny_tags,priority)
		VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),
		        $6,$7,$8,$9,$10,$11,$12,$13)`,
		rid, providerID, in.RuleType,
		in.Pattern, in.ModelID,
		in.RequireStreaming, in.RequireTools, in.RequireVision,
		in.RequireAudio, in.RequireEmbeddings, in.RequireReasoning,
		denyTagsArray, in.Priority,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if h.resolver != nil {
		h.resolver.Invalidate()
	}
	c.JSON(http.StatusCreated, gin.H{"id": rid, "provider_id": providerID})
}

// DeleteRule handles DELETE /admin/v1/providers/:id/rules/:rid
func (h *CatalogHandler) DeleteRule(c *gin.Context) {
	rid := c.Param("rid")
	_, _ = h.db.ExecContext(c.Request.Context(),
		`DELETE FROM provider_exposure_rules WHERE id::text=$1`, rid)
	if h.resolver != nil {
		h.resolver.Invalidate()
	}
	c.JSON(http.StatusOK, gin.H{"message": "rule deleted", "id": rid})
}

// PreviewRules handles POST /admin/v1/providers/:id/rules/preview
func (h *CatalogHandler) PreviewRules(c *gin.Context) {
	id := c.Param("id")
	exposed, blocked, err := h.rules.PreviewExposure(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"exposed_count": len(exposed),
		"blocked_count": len(blocked),
		"exposed":       exposed,
		"blocked":       blocked,
	})
}

// BulkRegisterFromCatalog handles POST /admin/v1/providers/:id/register-models
//
// This is the unified path that replaces team_virtual_model_permissions.
//
// It promotes one or more Mode-B catalog models into Mode-A Public Models by
// creating a models + model_endpoints + model_versions row for each entry.
// Once registered, the models are first-class Public Models and can be granted
// to teams via the standard team_model_permissions table — identical to local
// models. There is no separate permission table for remote models.
//
// Request body:
//
//	{
//	  "models": [
//	    {
//	      "public_name": "company-gpt",       // required — the public model name
//	      "provider_model_id": "openai/gpt-4o", // required — must exist in catalog
//	      "display_name": "Company GPT",       // optional
//	      "service_type": "CHAT"               // optional, default CHAT
//	    }
//	  ]
//	}
//
// Each entry is independent — failures are collected, successes proceed.
// Returns a summary of created model IDs and any errors.
func (h *CatalogHandler) BulkRegisterFromCatalog(c *gin.Context) {
	providerID := c.Param("id")

	type modelInput struct {
		PublicName      string `json:"public_name"`
		ProviderModelID string `json:"provider_model_id"`
		DisplayName     string `json:"display_name"`
		ServiceType     string `json:"service_type"`
	}
	var in struct {
		Models []modelInput `json:"models" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(in.Models) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "models must not be empty"})
		return
	}

	ctx := c.Request.Context()

	// Load provider once — all models in this request share the same provider.
	p, err := h.store.Get(ctx, providerID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider not found: " + providerID})
		return
	}

	type result struct {
		PublicName      string `json:"public_name"`
		ProviderModelID string `json:"provider_model_id"`
		ModelID         string `json:"model_id,omitempty"`
		EndpointID      string `json:"endpoint_id,omitempty"`
		Error           string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(in.Models))

	for _, entry := range in.Models {
		if entry.PublicName == "" || entry.ProviderModelID == "" {
			results = append(results, result{
				PublicName:      entry.PublicName,
				ProviderModelID: entry.ProviderModelID,
				Error:           "public_name and provider_model_id are required",
			})
			continue
		}
		if entry.ServiceType == "" {
			entry.ServiceType = "CHAT"
		}
		displayName := entry.DisplayName
		if displayName == "" {
			displayName = entry.PublicName
		}

		// Verify the catalog entry exists.
		var catalogID, catalogDisplayName string
		lookupErr := h.db.QueryRowContext(ctx,
			`SELECT id::text, display_name FROM provider_remote_models
			 WHERE provider_id::text=$1 AND provider_model_id=$2 LIMIT 1`,
			p.ID, entry.ProviderModelID,
		).Scan(&catalogID, &catalogDisplayName)
		if lookupErr != nil {
			results = append(results, result{
				PublicName:      entry.PublicName,
				ProviderModelID: entry.ProviderModelID,
				Error:           "catalog entry not found: " + entry.ProviderModelID,
			})
			continue
		}
		if displayName == entry.PublicName && catalogDisplayName != "" {
			displayName = catalogDisplayName
		}

		// Default capabilities by service type.
		caps, _ := marshalJSON([]string{"chat", "completion"})
		if entry.ServiceType == "EMBEDDING" {
			caps, _ = marshalJSON([]string{"embedding"})
		}

		mID := uuid.New().String()
		_, insertErr := h.db.ExecContext(ctx, `
			INSERT INTO models
			  (id, name, display_name, provider, backend_type, service_type,
			   max_context, max_output, enabled, tags, capabilities,
			   provider_name, provider_is_external, provider_id, provider_catalog_id)
			VALUES ($1,$2,$3,$4,$5,$6,128000,16384,TRUE,'[]'::jsonb,$7::jsonb,
			        $4,TRUE,$8::uuid,$9::uuid)`,
			mID, entry.PublicName, displayName,
			p.BackendType, p.BackendType, entry.ServiceType,
			caps, p.ID, catalogID,
		)
		if insertErr != nil {
			results = append(results, result{
				PublicName:      entry.PublicName,
				ProviderModelID: entry.ProviderModelID,
				Error:           "model already exists or DB error: " + insertErr.Error(),
			})
			continue
		}

		_, _ = h.db.ExecContext(ctx,
			`INSERT INTO model_versions (id,model_id,version,is_default) VALUES ($1,$2,'v1',TRUE)`,
			uuid.New().String(), mID)

		// Restore team_model_permissions from snapshot if this model name was previously
		// soft-deleted.  No-op on first-ever registration.
		if _, restoreErr := restorePermissionsFromSnapshot(ctx, h.db, h.engine, h.log, entry.PublicName, mID); restoreErr != nil {
			h.log.Error("permission restore failed — this registration will NOT have any team grants "+
				"automatically recovered from a previous deletion of this model name; grant access manually if needed",
				zap.String("model_name", entry.PublicName), zap.String("model_id", mID), zap.Error(restoreErr))
		}

		epID := uuid.New().String()
		_, _ = h.db.ExecContext(ctx, `
			INSERT INTO model_endpoints
			  (id, model_id, host, port, base_path, weight, priority,
			   health_status, is_enabled, lifecycle_state,
			   upstream_api_key, upstream_base_url, upstream_proxy, upstream_model_name,
			   provider_proxy_url)
			VALUES ($1,$2,'0.0.0.0',0,'/v1',100,1,'unknown',TRUE,'active',
			        NULLIF($3,''), NULLIF($4,''), '', NULLIF($5,''), NULLIF($6,''))`,
			epID, mID, p.APIKey, p.BaseURL, entry.ProviderModelID, p.ProxyURL,
		)

		results = append(results, result{
			PublicName:      entry.PublicName,
			ProviderModelID: entry.ProviderModelID,
			ModelID:         mID,
			EndpointID:      epID,
		})
	}

	if h.registry != nil {
		_ = h.registry.Reload(ctx)
	}

	created := 0
	for _, r := range results {
		if r.Error == "" {
			created++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"created": created,
		"total":   len(in.Models),
		"results": results,
		"note": "Registered models are now first-class Public Models. " +
			"Grant team access via POST /admin/v1/teams/:id/models.",
	})
}

// ── Project Provider Access (Catalog / Hybrid mode) ──────────────────────────
//
// These endpoints manage the project_provider_access table, which controls
// which projects may call virtual (catalog/hybrid) models from a provider.
// They are orthogonal to team_model_permissions — the latter covers Public
// Models (managed mode), while these cover the dynamic catalog path.

// ListProjectProviderAccess handles GET /admin/v1/projects/:id/provider-access
func (h *CatalogHandler) ListProjectProviderAccess(c *gin.Context) {
	projectID := c.Param("id")
	store := catalog.NewProjectProviderAccessStore(h.db)
	grants, err := store.ListForProject(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": grants, "total": len(grants), "project_id": projectID})
}

// GrantProjectProviderAccess handles POST /admin/v1/projects/:id/provider-access
//
// Grants a project access to a provider's virtual catalog models.
// Request body:
//
//	{
//	  "provider_id":      "uuid",
//	  "allowed_prefixes": ["openrouter/openai/*"],   // optional; empty = allow all
//	  "denied_prefixes":  ["openrouter/openai/gpt-4-*"] // optional
//	}
func (h *CatalogHandler) GrantProjectProviderAccess(c *gin.Context) {
	projectID := c.Param("id")
	var in struct {
		ProviderID      string   `json:"provider_id" binding:"required"`
		AllowedPrefixes []string `json:"allowed_prefixes"`
		DeniedPrefixes  []string `json:"denied_prefixes"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Verify the provider exists and is in catalog/hybrid mode.
	p, err := h.store.Get(c.Request.Context(), in.ProviderID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider not found: " + in.ProviderID})
		return
	}
	if !p.ExposureMode.IsVirtual() {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "provider '" + p.Name + "' is in managed mode — " +
				"set exposure_mode to 'catalog' or 'hybrid' before granting project access",
		})
		return
	}

	// Upsert the access grant.
	allowArr := pq.Array(in.AllowedPrefixes)
	denyArr := pq.Array(in.DeniedPrefixes)
	grantID := uuid.New().String()
	_, err = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO project_provider_access
		  (id, project_id, provider_id, allowed_prefixes, denied_prefixes)
		VALUES ($1, $2::uuid, $3::uuid, $4, $5)
		ON CONFLICT (project_id, provider_id) DO UPDATE
		SET allowed_prefixes = EXCLUDED.allowed_prefixes,
		    denied_prefixes  = EXCLUDED.denied_prefixes,
		    enabled          = TRUE,
		    updated_at       = NOW()`,
		grantID, projectID, p.ID, allowArr, denyArr,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "DB error: " + err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":               grantID,
		"project_id":       projectID,
		"provider_id":      p.ID,
		"provider_name":    p.Name,
		"exposure_mode":    string(p.ExposureMode),
		"allowed_prefixes": in.AllowedPrefixes,
		"denied_prefixes":  in.DeniedPrefixes,
		"note": "Project can now call virtual models from this provider. " +
			"Redis ACL will be refreshed on the next gateway reload cycle (≤60s) " +
			"or by calling POST /admin/v1/seed-permissions.",
	})
}

// UpdateProjectProviderAccess handles PUT /admin/v1/projects/:id/provider-access/:provider_id
//
// Updates the prefix filters on an existing grant.
func (h *CatalogHandler) UpdateProjectProviderAccess(c *gin.Context) {
	projectID := c.Param("id")
	providerID := c.Param("provider_id")
	var in struct {
		AllowedPrefixes []string `json:"allowed_prefixes"`
		DeniedPrefixes  []string `json:"denied_prefixes"`
		Enabled         *bool    `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	if in.AllowedPrefixes != nil {
		_, _ = h.db.ExecContext(ctx,
			`UPDATE project_provider_access SET allowed_prefixes=$3, updated_at=NOW()
			 WHERE project_id::text=$1 AND provider_id::text=$2`,
			projectID, providerID, pq.Array(in.AllowedPrefixes))
	}
	if in.DeniedPrefixes != nil {
		_, _ = h.db.ExecContext(ctx,
			`UPDATE project_provider_access SET denied_prefixes=$3, updated_at=NOW()
			 WHERE project_id::text=$1 AND provider_id::text=$2`,
			projectID, providerID, pq.Array(in.DeniedPrefixes))
	}
	if in.Enabled != nil {
		_, _ = h.db.ExecContext(ctx,
			`UPDATE project_provider_access SET enabled=$3, updated_at=NOW()
			 WHERE project_id::text=$1 AND provider_id::text=$2`,
			projectID, providerID, *in.Enabled)
	}
	c.JSON(http.StatusOK, gin.H{
		"message":     "provider access updated",
		"project_id":  projectID,
		"provider_id": providerID,
	})
}

// RevokeProjectProviderAccess handles DELETE /admin/v1/projects/:id/provider-access/:provider_id
func (h *CatalogHandler) RevokeProjectProviderAccess(c *gin.Context) {
	projectID := c.Param("id")
	providerID := c.Param("provider_id")
	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE project_provider_access SET enabled=FALSE, updated_at=NOW()
		 WHERE project_id::text=$1 AND provider_id::text=$2`,
		projectID, providerID)
	c.JSON(http.StatusOK, gin.H{
		"message":     "provider access revoked",
		"project_id":  projectID,
		"provider_id": providerID,
	})
}

// ── Live Models passthrough ───────────────────────────────────────────────────

// LiveModels handles GET /admin/v1/providers/:id/live-models
//
// Proxies the provider's own /models endpoint using the stored API key and
// transport config (proxy, TLS, timeouts). Returns the raw provider JSON
// unchanged so the UI can display every provider-specific field.
//
// This is the correct architecture: the admin server holds the credentials
// and transport config; the browser never touches the API key directly.
// No client-side key or localStorage is needed.
func (h *CatalogHandler) LiveModels(c *gin.Context) {
	id := c.Param("id")
	p, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "provider not found: " + id})
		return
	}

	// Build the upstream /models URL using the same path logic as the
	// catalog syncer so the behaviour is identical.
	path := "/v1/models"
	switch runtime.BackendType(p.BackendType) {
	case runtime.BackendOpenRouter:
		path = "/api/v1/models"
	case runtime.BackendGroq:
		path = "/openai/v1/models"
	case runtime.BackendGemini:
		path = "/v1beta/openai/models"
	}
	modelsURL := runtime.NormalizeProviderEndpointURL(p.BaseURL, path)

	// Forward any query string the caller passed (e.g. ?supported_parameters=tools).
	if rawQuery := c.Request.URL.RawQuery; rawQuery != "" {
		modelsURL += "?" + rawQuery
	}

	// Build an isolated HTTP client from the provider's full transport config —
	// same client used for catalog sync and chat completions. This ensures the
	// outbound proxy and TLS settings are applied identically.
	client, clientErr := runtime.BuildProviderClient(p.Transport())
	if clientErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build transport: " + clientErr.Error()})
		return
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, modelsURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Inject the stored API key using the provider's configured header.
	if p.APIKey != "" {
		header := p.APIKeyHeader
		if header == "" || header == "Authorization" {
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
		} else {
			req.Header.Set(header, p.APIKey)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream error: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read upstream response: " + readErr.Error()})
		return
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	c.Header("X-Nexus-Provider", p.Name)
	c.Header("X-Nexus-Provider-URL", modelsURL)
	c.Data(resp.StatusCode, ct, body)
}
