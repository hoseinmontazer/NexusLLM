package catalog

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
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

// fetchModels calls the provider's Models endpoint and returns a slice of RemoteModel.
func (s *CatalogSyncer) fetchModels(ctx context.Context, p *Provider, client *http.Client) ([]RemoteModel, error) {
	bt := runtime.BackendType(p.BackendType)
	backend, err := s.factory.Build(bt)
	if err != nil {
		// Unknown backend type — fall back to openai_compat models endpoint.
		backend, err = s.factory.Build(runtime.BackendOpenAICompat)
		if err != nil {
			return nil, err
		}
	}

	rawModels, err := backend.Models(ctx, p.BaseURL, client)
	if err != nil {
		return nil, err
	}

	out := make([]RemoteModel, 0, len(rawModels))
	for _, m := range rawModels {
		rm := RemoteModel{
			ProviderModelID:  m.ID,
			DisplayName:      m.ID,
			SupportsStreaming: true,
			Tags:             extractTags(m.ID),
			Metadata:         map[string]interface{}{"object": m.Object, "owned_by": m.OwnedBy},
		}
		// Infer capabilities from tags and model ID keywords.
		rm.SupportsTools      = containsAny(m.ID, "gpt-4", "gpt-3.5", "claude", "gemini", "llama", "mistral", "qwen", "deepseek")
		rm.SupportsVision     = containsAny(m.ID, "vision", "vl", "visual", "image", "4o", "gemini", "claude-3")
		rm.SupportsEmbedding  = containsAny(m.ID, "embed", "embedding", "e5", "bge", "minilm")
		rm.SupportsAudio      = containsAny(m.ID, "whisper", "audio", "speech", "tts", "stt")
		rm.SupportsReasoning  = containsAny(m.ID, "o1", "o3", "o4", "r1", "thinking", "reason", "cot")
		rm.SupportsImages     = containsAny(m.ID, "dall-e", "stable-diffusion", "flux", "imagen", "sdxl")
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
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		seen[m.ProviderModelID] = true

		tagsStr := "{}"
		if len(m.Tags) > 0 {
			tagsStr = `{"` + strings.Join(m.Tags, `","`) + `"}`
		}

		_, err := tx.ExecContext(ctx, `
			INSERT INTO provider_remote_models
			  (provider_id, provider_model_id, display_name, description,
			   supports_streaming, supports_tools, supports_vision,
			   supports_audio, supports_embeddings, supports_reasoning, supports_images,
			   tags, enabled, last_seen_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::text[],TRUE,NOW())
			ON CONFLICT (provider_id, provider_model_id) DO UPDATE
			SET display_name       = EXCLUDED.display_name,
			    supports_streaming = EXCLUDED.supports_streaming,
			    supports_tools     = EXCLUDED.supports_tools,
			    supports_vision    = EXCLUDED.supports_vision,
			    supports_audio     = EXCLUDED.supports_audio,
			    supports_embeddings= EXCLUDED.supports_embeddings,
			    supports_reasoning = EXCLUDED.supports_reasoning,
			    supports_images    = EXCLUDED.supports_images,
			    tags               = EXCLUDED.tags,
			    enabled            = TRUE,
			    removed_at         = NULL,
			    last_seen_at       = NOW()`,
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
			tagsStr,
		)
		if err != nil {
			return err
		}
	}

	// Mark models not returned this sync as removed.
	if len(seen) > 0 {
		// Build exclusion list.
		ids := make([]string, 0, len(seen))
		for id := range seen {
			ids = append(ids, id)
		}
		// Use ANY(ARRAY[...]) to avoid N parameters.
		_, err = tx.ExecContext(ctx, `
			UPDATE provider_remote_models
			SET enabled=FALSE, removed_at=NOW()
			WHERE provider_id=$1
			  AND enabled=TRUE
			  AND provider_model_id != ALL($2::text[])`,
			providerID, "{"+strings.Join(quoteAll(ids), ",")+"}",
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

func containsAny(s string, keywords ...string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		// Escape single quotes for the PostgreSQL array literal.
		out[i] = `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
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
