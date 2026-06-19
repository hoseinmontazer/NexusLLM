// Package alias resolves virtual model names (e.g. "gpt-4o", "reasoning")
// to real registered model names before the request reaches the runtime registry.
// Resolution order: team alias → org alias → global alias → identity.
package alias

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
)

const aliasCacheTTL = 5 * time.Minute

// Resolver resolves model aliases.
type Resolver struct {
	db  *sqlx.DB
	rdb *redis.Client
}

// NewResolver constructs a Resolver.
func NewResolver(db *sqlx.DB, rdb *redis.Client) *Resolver {
	return &Resolver{db: db, rdb: rdb}
}

// Resolve returns the real model name for the given alias, scoped to a
// specific team and org. Falls through team → org → global → identity.
func (r *Resolver) Resolve(ctx context.Context, alias, teamID, orgID string) (string, error) {
	// 1. Team alias
	if name, err := r.lookup(ctx, alias, "team", teamID); err == nil {
		return name, nil
	}
	// 2. Org alias
	if name, err := r.lookup(ctx, alias, "org", orgID); err == nil {
		return name, nil
	}
	// 3. Global alias
	if name, err := r.lookup(ctx, alias, "global", ""); err == nil {
		return name, nil
	}
	// 4. Identity — assume the alias IS the real model name
	return alias, nil
}

// CreateAlias persists a new alias mapping.
func (r *Resolver) CreateAlias(ctx context.Context, alias, modelID, scope, scopeID string) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO model_aliases (alias, model_id, scope, scope_id, enabled)
		VALUES ($1, $2, $3, $4, TRUE)
		ON CONFLICT (alias, scope, scope_id) DO UPDATE SET model_id = EXCLUDED.model_id`,
		alias, modelID, scope, nullableID(scopeID),
	)
	if err == nil {
		r.invalidate(ctx, alias, scope, scopeID)
	}
	return err
}

// DeleteAlias removes an alias.
func (r *Resolver) DeleteAlias(ctx context.Context, alias, scope, scopeID string) error {
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM model_aliases WHERE alias = $1 AND scope = $2 AND scope_id IS NOT DISTINCT FROM $3`,
		alias, scope, nullableID(scopeID),
	)
	r.invalidate(ctx, alias, scope, scopeID)
	return err
}

// ListAliases returns all aliases visible to a team/org.
func (r *Resolver) ListAliases(ctx context.Context, teamID, orgID string) ([]AliasRow, error) {
	var rows []AliasRow
	err := r.db.SelectContext(ctx, &rows, `
		SELECT ma.alias, m.name AS model_name, ma.scope, ma.scope_id, ma.enabled
		FROM model_aliases ma
		JOIN models m ON m.id = ma.model_id
		WHERE ma.enabled = TRUE
		  AND (
		       (ma.scope = 'global')
		    OR (ma.scope = 'org'   AND ma.scope_id = $1)
		    OR (ma.scope = 'team'  AND ma.scope_id = $2)
		      )
		ORDER BY ma.scope, ma.alias`, orgID, teamID)
	return rows, err
}

// AliasRow is the response shape for listing aliases.
type AliasRow struct {
	Alias     string  `db:"alias"      json:"alias"`
	ModelName string  `db:"model_name" json:"model_name"`
	Scope     string  `db:"scope"      json:"scope"`
	ScopeID   *string `db:"scope_id"   json:"scope_id"`
	Enabled   bool    `db:"enabled"    json:"enabled"`
}

// ─── private ──────────────────────────────────────────────────────────────────

func (r *Resolver) lookup(ctx context.Context, alias, scope, scopeID string) (string, error) {
	cacheKey := fmt.Sprintf("nexus:alias:%s:%s:%s", scope, scopeID, alias)

	// Redis cache
	if cached, err := r.rdb.Get(ctx, cacheKey).Result(); err == nil {
		return cached, nil
	}

	// DB lookup
	var modelName string
	var err error
	if scope == "global" {
		err = r.db.GetContext(ctx, &modelName, `
			SELECT m.name FROM model_aliases ma
			JOIN models m ON m.id = ma.model_id
			WHERE ma.alias = $1 AND ma.scope = 'global' AND ma.enabled = TRUE
			LIMIT 1`, alias)
	} else {
		err = r.db.GetContext(ctx, &modelName, `
			SELECT m.name FROM model_aliases ma
			JOIN models m ON m.id = ma.model_id
			WHERE ma.alias = $1 AND ma.scope = $2 AND ma.scope_id = $3 AND ma.enabled = TRUE
			LIMIT 1`, alias, scope, scopeID)
	}
	if err != nil {
		return "", err
	}

	_ = r.rdb.Set(ctx, cacheKey, modelName, aliasCacheTTL).Err()
	return modelName, nil
}

func (r *Resolver) invalidate(ctx context.Context, alias, scope, scopeID string) {
	key := fmt.Sprintf("nexus:alias:%s:%s:%s", scope, scopeID, alias)
	_ = r.rdb.Del(ctx, key).Err()
}

func nullableID(id string) interface{} {
	if id == "" {
		return nil
	}
	return id
}
