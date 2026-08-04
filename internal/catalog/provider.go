// Package catalog implements the four-layer Provider Catalog Architecture.
//
// Layer 1 — Provider: connection config per cloud provider (this file)
// Layer 2 — Remote Catalog: synced model mirror (syncer.go)
// Layer 3 — Exposure Rules: allow/deny engine (rules.go)
// Layer 4 — Virtual resolver: Mode B gateway dispatch (resolver.go)
package catalog

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/runtime"
)

// ExposureMode controls how a provider's models are surfaced to gateway clients.
type ExposureMode string

const (
	// ExposureManaged is the default, existing behaviour.
	// Administrators explicitly register Public Models via RegisterCatalogAlias
	// or BulkRegisterFromCatalog. Only those models appear in GET /v1/models.
	// Authorization: team_model_permissions (unchanged).
	ExposureManaged ExposureMode = "managed"

	// ExposureCatalog exposes the provider catalogue as virtual models directly.
	// No Public Model rows are required. GET /v1/models returns virtual names of
	// the form <prefix>/<provider_model_id>. Authorization: project_provider_access.
	// Every request still passes through the full policy / rate-limit / quota pipeline.
	ExposureCatalog ExposureMode = "catalog"

	// ExposureHybrid combines both modes simultaneously.
	// Registered Public Models AND virtual catalog models are visible and callable.
	// Authorization: team_model_permissions for Public Models,
	//               project_provider_access for virtual catalog models.
	ExposureHybrid ExposureMode = "hybrid"
)

// IsVirtual returns true when the exposure mode causes the provider catalogue
// to be surfaced as virtual models (Catalog or Hybrid).
func (m ExposureMode) IsVirtual() bool {
	return m == ExposureCatalog || m == ExposureHybrid
}

// Provider is the Layer-1 struct. It holds every field needed to reach
// a cloud provider: credentials, transport, sync config, and health state.
// One provider → many catalog entries. One provider → one *http.Client.
type Provider struct {
	ID           string `db:"id"`
	Name         string `db:"name"`
	DisplayName  string `db:"display_name"`
	BackendType  string `db:"backend_type"`
	BaseURL      string `db:"base_url"`
	APIKey       string `db:"api_key"`
	APIKeyHeader string `db:"api_key_header"`

	// ExposureMode controls how this provider's models are surfaced.
	// Migration 050 adds this column; existing rows default to "managed".
	// catalog_direct_expose is kept in sync by a DB trigger and must NOT be
	// written directly — always update ExposureMode instead.
	ExposureMode ExposureMode `db:"exposure_mode"`

	CatalogSyncEnabled  bool       `db:"catalog_sync_enabled"`
	CatalogSyncInterval int        `db:"catalog_sync_interval"` // seconds
	CatalogDirectExpose bool       `db:"catalog_direct_expose"`
	CatalogExposePrefix string     `db:"catalog_expose_prefix"`
	CatalogLastSyncedAt *time.Time `db:"catalog_last_synced_at"`
	CatalogModelCount   int        `db:"catalog_model_count"`
	CatalogSyncStatus   string     `db:"catalog_sync_status"`
	CatalogSyncError    string     `db:"catalog_sync_error"`

	// Transport mirrors migration 046/047 columns.
	ProxyURL                     string `db:"proxy_url"`
	TLSInsecureSkipVerify        bool   `db:"tls_insecure_skip_verify"`
	TLSRootCAPEM                 string `db:"tls_root_ca_pem"`
	ConnectTimeoutSeconds        int    `db:"connect_timeout_seconds"`
	ReadTimeoutSeconds           int    `db:"read_timeout_seconds"`
	IdleConnTimeoutSeconds       int    `db:"idle_conn_timeout_seconds"`
	ResponseHeaderTimeoutSeconds int    `db:"response_header_timeout_seconds"`
	MaxIdleConnsPerHost          int    `db:"max_idle_conns_per_host"`
	MaxConnsPerHost              int    `db:"max_conns_per_host"`
	DisableHTTP2                 bool   `db:"disable_http2"`

	RequestTimeoutSeconds int `db:"request_timeout_seconds"`
	MaxRetries            int `db:"max_retries"`

	Enabled         bool       `db:"enabled"`
	Health          string     `db:"health"`
	LastHealthCheck *time.Time `db:"last_health_check"`
	CreatedAt       time.Time  `db:"created_at"`
	UpdatedAt       time.Time  `db:"updated_at"`
}

// Transport converts provider DB columns to a ProviderTransportConfig.
func (p *Provider) Transport() runtime.ProviderTransportConfig {
	return runtime.ProviderTransportConfig{
		ProxyURL:                     p.ProxyURL,
		TLSInsecureSkipVerify:        p.TLSInsecureSkipVerify,
		TLSRootCAPEM:                 p.TLSRootCAPEM,
		ConnectTimeoutSeconds:        p.ConnectTimeoutSeconds,
		ReadTimeoutSeconds:           p.ReadTimeoutSeconds,
		IdleConnTimeoutSeconds:       p.IdleConnTimeoutSeconds,
		ResponseHeaderTimeoutSeconds: p.ResponseHeaderTimeoutSeconds,
		MaxIdleConnsPerHost:          p.MaxIdleConnsPerHost,
		MaxConnsPerHost:              p.MaxConnsPerHost,
		DisableHTTP2:                 p.DisableHTTP2,
	}
}

// ProviderStore handles DB read/write for providers.
type ProviderStore struct {
	db *sqlx.DB
}

// NewProviderStore constructs a ProviderStore.
func NewProviderStore(db *sqlx.DB) *ProviderStore {
	return &ProviderStore{db: db}
}

// providerSelectCols is the canonical SELECT column list used in all
// ProviderStore queries. It must stay in sync with the Provider struct.
const providerSelectCols = `
	id::text, name, display_name, backend_type, base_url,
	api_key, api_key_header,
	COALESCE(exposure_mode,'managed') AS exposure_mode,
	catalog_sync_enabled, catalog_sync_interval,
	catalog_direct_expose, catalog_expose_prefix,
	catalog_last_synced_at, catalog_model_count,
	catalog_sync_status, COALESCE(catalog_sync_error,'') AS catalog_sync_error,
	COALESCE(proxy_url,'') AS proxy_url,
	tls_insecure_skip_verify, COALESCE(tls_root_ca_pem,'') AS tls_root_ca_pem,
	connect_timeout_seconds, read_timeout_seconds,
	idle_conn_timeout_seconds, response_header_timeout_seconds,
	max_idle_conns_per_host, max_conns_per_host, disable_http2,
	request_timeout_seconds, max_retries,
	enabled, health, last_health_check, created_at, updated_at`

// List returns all providers, ordered by name.
func (s *ProviderStore) List(ctx context.Context) ([]Provider, error) {
	var rows []Provider
	err := s.db.SelectContext(ctx, &rows,
		`SELECT `+providerSelectCols+` FROM providers ORDER BY name`)
	return rows, err
}

// Get returns a single provider by UUID or name.
func (s *ProviderStore) Get(ctx context.Context, idOrName string) (*Provider, error) {
	var p Provider
	err := s.db.GetContext(ctx, &p,
		`SELECT `+providerSelectCols+`
		 FROM providers
		 WHERE id::text = $1 OR name = $1
		 LIMIT 1`, idOrName)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListSyncEnabled returns all enabled providers with catalog sync on.
func (s *ProviderStore) ListSyncEnabled(ctx context.Context) ([]Provider, error) {
	var rows []Provider
	err := s.db.SelectContext(ctx, &rows,
		`SELECT `+providerSelectCols+`
		 FROM providers
		 WHERE enabled = TRUE AND catalog_sync_enabled = TRUE
		 ORDER BY name`)
	return rows, err
}

// ListVirtual returns all enabled providers whose exposure_mode is 'catalog'
// or 'hybrid'. These are the providers whose catalogue is directly surfaced
// as virtual model names in GET /v1/models and the inference hot-path.
func (s *ProviderStore) ListVirtual(ctx context.Context) ([]Provider, error) {
	var rows []Provider
	err := s.db.SelectContext(ctx, &rows,
		`SELECT `+providerSelectCols+`
		 FROM providers
		 WHERE enabled = TRUE AND exposure_mode IN ('catalog', 'hybrid')
		 ORDER BY name`)
	return rows, err
}

// UpdateExposureMode sets exposure_mode on a provider row. The DB trigger
// will automatically update catalog_direct_expose accordingly.
func (s *ProviderStore) UpdateExposureMode(ctx context.Context, id string, mode ExposureMode) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE providers SET exposure_mode=$2, updated_at=NOW() WHERE id::text=$1`,
		id, string(mode))
	return err
}

func (s *ProviderStore) UpdateHealth(ctx context.Context, id, health string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE providers SET health=$2, last_health_check=NOW(), updated_at=NOW()
		 WHERE id::text=$1`, id, health)
	return err
}

// UpdateSyncStatus updates catalog sync metadata.
func (s *ProviderStore) UpdateSyncStatus(ctx context.Context, id, status, errMsg string, count int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE providers
		SET catalog_sync_status=$2,
		    catalog_sync_error=$3,
		    catalog_last_synced_at=NOW(),
		    catalog_model_count=$4,
		    updated_at=NOW()
		WHERE id::text=$1`, id, status, errMsg, count)
	return err
}

// MarkSyncing marks catalog_sync_status='syncing'.
func (s *ProviderStore) MarkSyncing(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE providers SET catalog_sync_status='syncing', updated_at=NOW() WHERE id::text=$1`, id)
	return err
}
