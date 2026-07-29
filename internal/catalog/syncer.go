package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"go.uber.org/zap"
)

// RemoteModel is a catalog entry fetched from a provider.
type RemoteModel struct {
	ProviderModelID   string
	DisplayName       string
	Description       string
	ContextLength     *int
	InputCostPer1M    *float64
	OutputCostPer1M   *float64
	SupportsStreaming  bool
	SupportsTools     bool
	SupportsVision    bool
	SupportsAudio     bool
	SupportsEmbedding bool
	SupportsReasoning bool
	SupportsImages    bool
	Tags              []string
	Metadata          map[string]interface{}
}

// CatalogSyncer synchronizes provider catalogs into provider_remote_models.
type CatalogSyncer struct {
	db      *sqlx.DB
	store   *ProviderStore
	factory *runtime.Factory
	log     *zap.Logger
}

// NewCatalogSyncer constructs a CatalogSyncer.
func NewCatalogSyncer(db *sqlx.DB, factory *runtime.Factory, log *zap.Logger) *CatalogSyncer {
	return &CatalogSyncer{
		db:      db,
		store:   NewProviderStore(db),
		factory: factory,
		log:     log,
	}
}

// SyncProvider performs an immediate catalog sync for one provider.
// It fetches /models from the provider, upserts rows, marks removed models
// disabled, and updates provider sync metadata.
func (s *CatalogSyncer) SyncProvider(ctx context.Context, providerID string) error {
	p, err := s.store.Get(ctx, providerID)
	if err != nil {
		return err
	}

	_ = s.store.MarkSyncing(ctx, p.ID)
	s.log.Info("catalog sync started", zap.String("provider", p.Name))

	// Build isolated HTTP client from provider transport config.
	client, err := runtime.BuildProviderClient(p.Transport())
	if err != nil {
		_ = s.store.UpdateSyncStatus(ctx, p.ID, "error", err.Error(), p.CatalogModelCount)
		return err
	}

	models, err := s.fetchModels(ctx, p, client)
	if err != nil {
		_ = s.store.UpdateSyncStatus(ctx, p.ID, "error", err.Error(), p.CatalogModelCount)
		s.log.Warn("catalog sync failed", zap.String("provider", p.Name), zap.Error(err))
		return err
	}

	if err := s.upsertCatalog(ctx, p.ID, models); err != nil {
		_ = s.store.UpdateSyncStatus(ctx, p.ID, "error", err.Error(), p.CatalogModelCount)
		return err
	}

	// Count enabled models.
	var count int
	_ = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM provider_remote_models WHERE provider_id=$1 AND enabled=TRUE`,
		p.ID).Scan(&count)

	_ = s.store.UpdateSyncStatus(ctx, p.ID, "ok", "", count)
	s.log.Info("catalog sync complete",
		zap.String("provider", p.Name),
		zap.Int("models", len(models)),
		zap.Int("enabled", count),
	)
	return nil
}

// fetchModels calls the provider's /models endpoint and returns a slice of RemoteModel.
// It calls the endpoint directly with the provider's API key rather than going through
// the Backend.Models() method, which does not accept an api key parameter.
func (s *CatalogSyncer) fetchModels(ctx context.Context, p *Provider, client *http.Client) ([]RemoteModel, error) {
	// Determine the correct models URL based on backend type.
	// These mirror the path logic in each provider's Models() implementation.
	modelsURL := p.BaseURL
	switch runtime.BackendType(p.BackendType) {
	case runtime.BackendOpenRouter:
		// OpenRouter: /api/v1/models
		modelsURL = strings.TrimSuffix(p.BaseURL, "/") + "/api/v1/models"
	case runtime.BackendGroq:
		// Groq: /openai/v1/models
		modelsURL = strings.TrimSuffix(p.BaseURL, "/") + "/openai/v1/models"
	case runtime.BackendGemini:
		// Gemini: /v1beta/openai/models
		modelsURL = strings.TrimSuffix(p.BaseURL, "/") + "/v1beta/openai/models"
	default:
		// All others: standard /v1/models
		modelsURL = strings.TrimSuffix(p.BaseURL, "/") + "/v1/models"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}

	// Inject the API key — this is the critical fix. The Backend.Models() interface
	// does not pass the key through, so calling it via the backend would send an
	// unauthenticated request that returns a Cloudflare block page (HTML).
	if p.APIKey != "" {
		header := p.APIKeyHeader
		if header == "" {
			header = "Authorization"
		}
		if header == "Authorization" {
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
		} else {
			req.Header.Set(header, p.APIKey)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("provider /models returned HTTP %d", resp.StatusCode)
	}

	type modelListResponse struct {
		Data []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	var list modelListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("provider /models response not valid JSON: %w", err)
	}

	out := make([]RemoteModel, 0, len(list.Data))
	for _, m := range list.Data {
		// Capability flags are NOT inferred from the model ID string.
		//
		// Architecture rule: routing and capability decisions must be based on
		// explicit metadata, never on pattern-matching names. The basic
		// /v1/models response carries no structured capability data — only id,
		// object, created, and owned_by. Storing guesses as facts would cause
		// incorrect routing (e.g. "openai/gpt-oss-20b" falsely claiming vision
		// because the ID contains "4o", or being treated as a local runtime
		// because the name looks like a local model).
		//
		// All capability flags default to false. Operators update them via:
		//   PUT /admin/v1/providers/:id/models/:model_id
		// or through a richer provider-specific sync that returns capability
		// metadata (e.g. OpenRouter's /api/v1/models includes context_length,
		// pricing, and supported parameters — that data can be used when the
		// provider exposes it explicitly).
		//
		// SupportsStreaming is the only safe default to set true — every chat
		// model endpoint advertised by the providers we integrate with supports
		// SSE streaming. The ON CONFLICT … DO UPDATE in upsertCatalog preserves
		// values for existing rows, so hand-edited flags are never clobbered.
		rm := RemoteModel{
			ProviderModelID:  m.ID,
			DisplayName:      m.ID,
			SupportsStreaming: true,
			Tags:             extractTags(m.ID),
			Metadata:         map[string]interface{}{"object": m.Object, "owned_by": m.OwnedBy},
		}
		out = append(out, rm)
	}
	return out, nil
}

// upsertCatalog inserts/updates catalog rows and marks removed models disabled.
func (s *CatalogSyncer) upsertCatalog(ctx context.Context, providerID string, models []RemoteModel) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Track model IDs returned this sync.
	seen := make([]string, 0, len(models))
	for _, m := range models {
		seen = append(seen, m.ProviderModelID)

		// Use pq.Array for the tags column — correctly handles all characters
		// including slashes, hyphens, colons in model IDs. Never breaks on
		// special characters the way manual literal construction does.
		var tagsArray interface{}
		if len(m.Tags) > 0 {
			tagsArray = pq.Array(m.Tags)
		} else {
			tagsArray = pq.Array([]string{})
		}

		// On conflict we intentionally do NOT overwrite supports_* capability
		// flags. Those are set once on first insert (all false for a basic
		// /v1/models response) and then managed exclusively by the operator via
		// PUT /admin/v1/providers/:id/models/:model_id. Re-syncing must never
		// reset hand-edited capability data back to the conservative defaults.
		// Only display metadata (display_name, tags, last_seen_at, enabled) is
		// refreshed from the provider on every sync.
		_, err := tx.ExecContext(ctx, `
			INSERT INTO provider_remote_models
			  (provider_id, provider_model_id, display_name, description,
			   supports_streaming, supports_tools, supports_vision,
			   supports_audio, supports_embeddings, supports_reasoning, supports_images,
			   tags, enabled, last_seen_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,TRUE,NOW())
			ON CONFLICT (provider_id, provider_model_id) DO UPDATE
			SET display_name = EXCLUDED.display_name,
			    tags         = EXCLUDED.tags,
			    enabled      = TRUE,
			    removed_at   = NULL,
			    last_seen_at = NOW()`,
			providerID,
			m.ProviderModelID,
			m.DisplayName,
			m.Description,
			m.SupportsStreaming,
			m.SupportsTools,
			m.SupportsVision,
			m.SupportsAudio,
			m.SupportsEmbedding,
			m.SupportsReasoning,
			m.SupportsImages,
			tagsArray,
		)
		if err != nil {
			return err
		}
	}

	// Mark models not returned this sync as removed.
	// Use pq.Array for the exclusion list — safe for model IDs with any characters.
	if len(seen) > 0 {
		_, err = tx.ExecContext(ctx, `
			UPDATE provider_remote_models
			SET enabled=FALSE, removed_at=NOW()
			WHERE provider_id=$1
			  AND enabled=TRUE
			  AND provider_model_id != ALL($2)`,
			providerID, pq.Array(seen),
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// extractTags parses a model ID and returns a slice of tags.
//
//	"openai/gpt-4o:free"     → ["free"]
//	"meta-llama/llama-3-70b-instruct" → ["instruct"]
func extractTags(modelID string) []string {
	tags := map[string]bool{}

	// Colon-suffix tags (openrouter style): "model:free", "model:nitro"
	parts := strings.SplitN(modelID, ":", 2)
	if len(parts) == 2 {
		suffix := strings.ToLower(parts[1])
		for _, t := range strings.Split(suffix, ":") {
			if t != "" {
				tags[t] = true
			}
		}
	}

	lower := strings.ToLower(modelID)
	keywords := []string{
		"preview", "beta", "instruct", "turbo", "mini", "nano",
		"vision", "audio", "thinking", "reasoning", "pro", "ultra",
	}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			tags[kw] = true
		}
	}

	out := make([]string, 0, len(tags))
	for t := range tags {
		out = append(out, t)
	}
	return out
}

// SyncInterval returns the provider's sync interval as a Duration.
func (s *CatalogSyncer) SyncInterval(p *Provider) time.Duration {
	if p.CatalogSyncInterval <= 0 {
		return 60 * time.Minute
	}
	return time.Duration(p.CatalogSyncInterval) * time.Second
}
