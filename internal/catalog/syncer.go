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
	ProviderModelID    string
	DisplayName        string
	Description        string
	ContextLength      *int
	MaxOutputTokens    *int
	InputCostPer1M     *float64
	OutputCostPer1M    *float64
	ProviderInputCost  *float64
	ProviderOutputCost *float64
	SupportsStreaming   bool
	SupportsTools      bool
	SupportsVision     bool
	SupportsAudio      bool
	SupportsEmbedding  bool
	SupportsReasoning  bool
	SupportsImages     bool
	SupportsJsonMode   bool
	Tags               []string
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
			ID          string  `json:"id"`
			Object      string  `json:"object"`
			Created     int64   `json:"created"`
			OwnedBy     string  `json:"owned_by"`
			Name        string  `json:"name"`
			Description string  `json:"description"`
			// OpenRouter and similar providers return rich metadata.
			ContextLength *int   `json:"context_length"`
			Architecture  *struct {
				Modality         string   `json:"modality"`
				InputModalities  []string `json:"input_modalities"`
				OutputModalities []string `json:"output_modalities"`
				Tokenizer        string   `json:"tokenizer"`
				InstructType     *string  `json:"instruct_type"`
			} `json:"architecture"`
			Pricing *struct {
				Prompt          string `json:"prompt"`
				Completion      string `json:"completion"`
				InputCacheRead  string `json:"input_cache_read"`
				InputCacheWrite string `json:"input_cache_write"`
				Image           string `json:"image"`
			} `json:"pricing"`
			TopProvider *struct {
				ContextLength       *int  `json:"context_length"`
				MaxCompletionTokens *int  `json:"max_completion_tokens"`
				IsModerated         bool  `json:"is_moderated"`
			} `json:"top_provider"`
			SupportedParameters []string               `json:"supported_parameters"`
			HuggingFaceID       *string                `json:"hugging_face_id"`
			CanonicalSlug       string                 `json:"canonical_slug"`
		} `json:"data"`
	}
	var list modelListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("provider /models response not valid JSON: %w", err)
	}

	out := make([]RemoteModel, 0, len(list.Data))
	for _, m := range list.Data {
		// Capability flags: derive from the architecture block when available
		// (OpenRouter provides it explicitly), otherwise fall back to safe
		// defaults. This avoids the forbidden pattern of inferring capabilities
		// from model name strings.
		supportsVision    := false
		supportsAudio     := false
		supportsEmbedding := false
		supportsImageGen  := false
		description       := m.Description
		if description == "" {
			description = m.Name
		}

		// Parse architecture input/output modalities when provided.
		if m.Architecture != nil {
			for _, mod := range m.Architecture.InputModalities {
				switch mod {
				case "image":
					supportsVision = true
				case "audio":
					supportsAudio = true
				}
			}
			for _, mod := range m.Architecture.OutputModalities {
				switch mod {
				case "image":
					supportsImageGen = true
				case "embedding":
					supportsEmbedding = true
				}
			}
		}

		// Derive supports_tools from supported_parameters when available.
		supportsTools    := false
		supportsJsonMode := false
		supportsReasoning := false
		for _, param := range m.SupportedParameters {
			switch param {
			case "tools", "tool_choice":
				supportsTools = true
			case "response_format":
				supportsJsonMode = true
			case "reasoning", "include_reasoning":
				supportsReasoning = true
			}
		}

		// Parse pricing into float64 per-1M-token values.
		var inputCost, outputCost *float64
		if m.Pricing != nil {
			if v, err := parseProviderCost(m.Pricing.Prompt); err == nil {
				inputCost = &v
			}
			if v, err := parseProviderCost(m.Pricing.Completion); err == nil {
				outputCost = &v
			}
		}

		// Context length from top_provider overrides the model-level field
		// when both are present (top_provider is per-instance, model is nominal).
		ctxLen := m.ContextLength
		var maxOutput *int
		if m.TopProvider != nil {
			if m.TopProvider.ContextLength != nil {
				ctxLen = m.TopProvider.ContextLength
			}
			maxOutput = m.TopProvider.MaxCompletionTokens
		}

		displayName := m.Name
		if displayName == "" {
			displayName = m.ID
		}

		rm := RemoteModel{
			ProviderModelID:   m.ID,
			DisplayName:       displayName,
			Description:       description,
			ContextLength:     ctxLen,
			MaxOutputTokens:   maxOutput,
			InputCostPer1M:    inputCost,
			OutputCostPer1M:   outputCost,
			ProviderInputCost: inputCost,
			ProviderOutputCost: outputCost,
			// SupportsStreaming defaults true — every chat model supports SSE.
			// The ON CONFLICT … DO UPDATE in upsertCatalog preserves existing
			// values so hand-edited flags are never clobbered on re-sync.
			SupportsStreaming:  true,
			SupportsTools:     supportsTools,
			SupportsVision:    supportsVision,
			SupportsAudio:     supportsAudio,
			SupportsEmbedding: supportsEmbedding,
			SupportsImages:    supportsImageGen,
			// Derived from supported_parameters.
			SupportsJsonMode:  supportsJsonMode,
			SupportsReasoning: supportsReasoning,
			Tags:              extractTags(m.ID),
			Metadata: map[string]interface{}{
				"object":               m.Object,
				"owned_by":             m.OwnedBy,
				"supported_parameters": m.SupportedParameters,
				"canonical_slug":       m.CanonicalSlug,
			},
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

		// On conflict, update display metadata AND the new rich columns
		// (context_length, pricing, description) from the provider's latest
		// /models response. Capability flags (supports_*) are still preserved
		// for existing rows — operators can override them manually and
		// re-syncing should not reset intentional overrides.
		// Exception: on first insert the capability flags come from the parsed
		// architecture block, giving better defaults than all-false.
		_, err := tx.ExecContext(ctx, `
			INSERT INTO provider_remote_models
			  (provider_id, provider_model_id, display_name, description,
			   context_length, max_output_tokens,
			   input_cost_per_1m, output_cost_per_1m,
			   provider_input_cost, provider_output_cost,
			   supports_streaming, supports_tools, supports_vision,
			   supports_audio, supports_embeddings, supports_reasoning, supports_images,
			   supports_json_mode,
			   tags, enabled, last_seen_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,TRUE,NOW())
			ON CONFLICT (provider_id, provider_model_id) DO UPDATE
			SET display_name         = EXCLUDED.display_name,
			    description          = EXCLUDED.description,
			    context_length       = EXCLUDED.context_length,
			    max_output_tokens    = COALESCE(EXCLUDED.max_output_tokens, provider_remote_models.max_output_tokens),
			    input_cost_per_1m    = COALESCE(EXCLUDED.input_cost_per_1m,  provider_remote_models.input_cost_per_1m),
			    output_cost_per_1m   = COALESCE(EXCLUDED.output_cost_per_1m, provider_remote_models.output_cost_per_1m),
			    provider_input_cost  = COALESCE(EXCLUDED.provider_input_cost,  provider_remote_models.provider_input_cost),
			    provider_output_cost = COALESCE(EXCLUDED.provider_output_cost, provider_remote_models.provider_output_cost),
			    tags                 = EXCLUDED.tags,
			    enabled              = TRUE,
			    removed_at           = NULL,
			    last_seen_at         = NOW()`,
			providerID,
			m.ProviderModelID,
			m.DisplayName,
			m.Description,
			m.ContextLength,
			m.MaxOutputTokens,
			m.InputCostPer1M,
			m.OutputCostPer1M,
			m.ProviderInputCost,
			m.ProviderOutputCost,
			m.SupportsStreaming,
			m.SupportsTools,
			m.SupportsVision,
			m.SupportsAudio,
			m.SupportsEmbedding,
			m.SupportsReasoning,
			m.SupportsImages,
			m.SupportsJsonMode,
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

// parseProviderCost converts a provider-reported cost string (e.g. "0.000001"
// or "1e-06") to a per-1M-token float64 value stored in input_cost_per_1m /
// output_cost_per_1m columns.  Returns an error for empty or unparseable values.
//
// OpenRouter reports costs as per-token strings (e.g. prompt="0.000001" means
// $1 per 1M tokens).  We store them as per-1M-token values for consistency with
// the existing schema.
func parseProviderCost(s string) (float64, error) {
	if s == "" || s == "0" {
		return 0, fmt.Errorf("zero or empty cost")
	}
	var v float64
	_, err := fmt.Sscanf(s, "%g", &v)
	if err != nil {
		return 0, err
	}
	// Convert per-token → per-1M-token.
	return v * 1_000_000, nil
}
