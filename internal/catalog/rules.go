package catalog

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/jmoiron/sqlx"
)

// ExposureRule is a Layer-3 allow/deny rule for a provider.
type ExposureRule struct {
	ID         string `db:"id"          json:"id"`
	ProviderID string `db:"provider_id" json:"provider_id"`
	RuleType   string `db:"rule_type"   json:"rule_type"`
	Pattern    string `db:"pattern"     json:"pattern"`
	ModelID    string `db:"model_id"    json:"model_id"`

	RequireStreaming *bool `db:"require_streaming"  json:"require_streaming"`
	RequireTools     *bool `db:"require_tools"      json:"require_tools"`
	RequireVision    *bool `db:"require_vision"     json:"require_vision"`
	RequireAudio     *bool `db:"require_audio"      json:"require_audio"`
	RequireEmbedding *bool `db:"require_embeddings" json:"require_embeddings"`
	RequireReasoning *bool `db:"require_reasoning"  json:"require_reasoning"`

	DenyTagsRaw string `db:"deny_tags_raw" json:"deny_tags_raw"`
	Priority    int    `db:"priority"      json:"priority"`
	Enabled     bool   `db:"enabled"       json:"enabled"`
}

// DenyTags splits DenyTagsRaw into a slice.
func (r *ExposureRule) DenyTags() []string {
	if r.DenyTagsRaw == "" {
		return nil
	}
	parts := strings.Split(r.DenyTagsRaw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// CatalogEntry is a minimal view of a catalog row used for rule evaluation.
type CatalogEntry struct {
	ProviderModelID   string
	Tags              []string
	SupportsStreaming bool
	SupportsTools     bool
	SupportsVision    bool
	SupportsAudio     bool
	SupportsEmbedding bool
	SupportsReasoning bool
}

// RuleEngine evaluates exposure rules against catalog entries.
// Default policy: deny everything not explicitly allowed.
type RuleEngine struct{}

// NewRuleEngine constructs a RuleEngine.
func NewRuleEngine() *RuleEngine { return &RuleEngine{} }

// IsExposed returns true when at least one allow rule matches the entry
// AND no deny rule fires first.
//
// Evaluation order:
//  1. Sort all rules by priority ASC.
//  2. Walk in order: deny_pattern / deny tag fires → return false immediately.
//  3. Walk again: allow_model exact / allow_pattern glob → check capability filter.
//  4. Default → false.
func (e *RuleEngine) IsExposed(entry CatalogEntry, rules []ExposureRule) bool {
	// Sort by priority ascending.
	sorted := make([]ExposureRule, len(rules))
	copy(sorted, rules)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Priority < sorted[j].Priority
	})

	// Pass 1: deny rules.
	for _, r := range sorted {
		if !r.Enabled {
			continue
		}
		switch r.RuleType {
		case "deny_pattern":
			if globMatch(r.Pattern, entry.ProviderModelID) {
				return false
			}
		}
		// Deny tags (applies to all rule types when deny_tags is set).
		for _, tag := range r.DenyTags() {
			for _, entryTag := range entry.Tags {
				if strings.EqualFold(tag, entryTag) {
					return false
				}
			}
		}
	}

	// Pass 2: allow rules.
	for _, r := range sorted {
		if !r.Enabled {
			continue
		}
		var matches bool
		switch r.RuleType {
		case "allow_model":
			matches = strings.EqualFold(r.ModelID, entry.ProviderModelID)
		case "allow_pattern":
			matches = globMatch(r.Pattern, entry.ProviderModelID)
		case "capability_filter":
			// capability_filter with no pattern matches all entries.
			matches = true
		}
		if matches && capabilityFilterPasses(r, entry) {
			return true
		}
	}

	return false // default deny
}

// capabilityFilterPasses returns true when the entry satisfies all
// require_* constraints declared on the rule.
func capabilityFilterPasses(r ExposureRule, entry CatalogEntry) bool {
	if r.RequireStreaming != nil && *r.RequireStreaming && !entry.SupportsStreaming {
		return false
	}
	if r.RequireTools != nil && *r.RequireTools && !entry.SupportsTools {
		return false
	}
	if r.RequireVision != nil && *r.RequireVision && !entry.SupportsVision {
		return false
	}
	if r.RequireAudio != nil && *r.RequireAudio && !entry.SupportsAudio {
		return false
	}
	if r.RequireEmbedding != nil && *r.RequireEmbedding && !entry.SupportsEmbedding {
		return false
	}
	if r.RequireReasoning != nil && *r.RequireReasoning && !entry.SupportsReasoning {
		return false
	}
	return true
}

// globMatch wraps path.Match so callers don't need to handle errors.
// Patterns use shell glob syntax: * matches anything except /, ** not supported.
// For provider model IDs like "openai/gpt-5" we match the whole string with
// path.Match which treats / as a path separator — so "openai/*" matches "openai/gpt-5".
func globMatch(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	matched, _ := path.Match(pattern, name)
	return matched
}

// ─────────────────────────────────────────────────────────────────────────────
// RuleStore — DB access for exposure rules
// ─────────────────────────────────────────────────────────────────────────────

// RuleStore handles DB operations for provider_exposure_rules.
type RuleStore struct {
	db *sqlx.DB
}

// NewRuleStore constructs a RuleStore.
func NewRuleStore(db *sqlx.DB) *RuleStore { return &RuleStore{db: db} }

// ListForProvider returns all enabled rules for a provider, sorted by priority.
func (s *RuleStore) ListForProvider(ctx context.Context, providerID string) ([]ExposureRule, error) {
	var rows []ExposureRule
	err := s.db.SelectContext(ctx, &rows, `
		SELECT id::text, provider_id::text, rule_type,
		       COALESCE(pattern,'')  AS pattern,
		       COALESCE(model_id,'') AS model_id,
		       require_streaming, require_tools, require_vision,
		       require_audio, require_embeddings, require_reasoning,
		       COALESCE(array_to_string(deny_tags,','),'') AS deny_tags_raw,
		       priority, enabled
		FROM provider_exposure_rules
		WHERE provider_id::text = $1 AND enabled = TRUE
		ORDER BY priority ASC`, providerID)
	return rows, err
}

// ListAll returns all rules for a provider regardless of enabled state.
func (s *RuleStore) ListAll(ctx context.Context, providerID string) ([]ExposureRule, error) {
	var rows []ExposureRule
	err := s.db.SelectContext(ctx, &rows, `
		SELECT id::text, provider_id::text, rule_type,
		       COALESCE(pattern,'')  AS pattern,
		       COALESCE(model_id,'') AS model_id,
		       require_streaming, require_tools, require_vision,
		       require_audio, require_embeddings, require_reasoning,
		       COALESCE(array_to_string(deny_tags,','),'') AS deny_tags_raw,
		       priority, enabled
		FROM provider_exposure_rules
		WHERE provider_id::text = $1
		ORDER BY priority ASC`, providerID)
	return rows, err
}

// PreviewExposure returns the IDs of catalog entries that would be exposed
// given the current rules. Used by the rule preview endpoint.
func (s *RuleStore) PreviewExposure(ctx context.Context, providerID string) (exposed, blocked []string, err error) {
	type row struct {
		ModelID           string `db:"provider_model_id"`
		Tags              string `db:"tags_raw"`
		SupportsStreaming bool   `db:"supports_streaming"`
		SupportsTools     bool   `db:"supports_tools"`
		SupportsVision    bool   `db:"supports_vision"`
		SupportsAudio     bool   `db:"supports_audio"`
		SupportsEmbedding bool   `db:"supports_embeddings"`
		SupportsReasoning bool   `db:"supports_reasoning"`
	}
	var catalog []row
	if err = s.db.SelectContext(ctx, &catalog, `
		SELECT provider_model_id,
		       COALESCE(array_to_string(tags,','),'') AS tags_raw,
		       supports_streaming, supports_tools, supports_vision,
		       supports_audio, supports_embeddings, supports_reasoning
		FROM provider_remote_models
		WHERE provider_id::text=$1 AND enabled=TRUE`, providerID); err != nil {
		return nil, nil, err
	}

	rules, err := s.ListForProvider(ctx, providerID)
	if err != nil {
		return nil, nil, err
	}

	engine := NewRuleEngine()
	for _, c := range catalog {
		entry := CatalogEntry{
			ProviderModelID:   c.ModelID,
			Tags:              splitTags(c.Tags),
			SupportsStreaming: c.SupportsStreaming,
			SupportsTools:     c.SupportsTools,
			SupportsVision:    c.SupportsVision,
			SupportsAudio:     c.SupportsAudio,
			SupportsEmbedding: c.SupportsEmbedding,
			SupportsReasoning: c.SupportsReasoning,
		}
		if engine.IsExposed(entry, rules) {
			exposed = append(exposed, c.ModelID)
		} else {
			blocked = append(blocked, c.ModelID)
		}
	}
	return exposed, blocked, nil
}

func splitTags(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}
