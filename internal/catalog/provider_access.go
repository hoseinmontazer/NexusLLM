package catalog

// provider_access.go — Layer-1b: project-level provider authorization.
//
// In Catalog and Hybrid exposure modes, individual model registration is not
// required. Authorization moves from the model level (team_model_permissions)
// to the provider level: a project is granted access to an entire provider
// catalogue, optionally constrained by allowed/denied prefix patterns.
//
// Architecture:
//   • ProjectProviderAccessStore  — DB read/write for project_provider_access
//   • ProjectProviderAccess       — a single grant row (project → provider)
//
// The hot-path (policy engine Evaluate) reads from Redis, not this store.
// The store is used only at:
//   1. Admin CRUD (create/update/delete grants)
//   2. Gateway startup and 60s reload (seeding Redis from DB)

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// ProjectProviderAccess is one row from project_provider_access.
// It grants a project permission to call catalog/hybrid virtual models from
// a specific provider, optionally filtered by prefix patterns.
type ProjectProviderAccess struct {
	ID              string    `db:"id"`
	ProjectID       string    `db:"project_id"`
	ProviderID      string    `db:"provider_id"`
	ProviderName    string    `db:"provider_name"`   // joined from providers
	ExposureMode    string    `db:"exposure_mode"`   // joined from providers
	AllowedPrefixes []string  `db:"allowed_prefixes"` // empty = allow all
	DeniedPrefixes  []string  `db:"denied_prefixes"`  // empty = deny none
	Enabled         bool      `db:"enabled"`
	CreatedAt       time.Time `db:"created_at"`
	UpdatedAt       time.Time `db:"updated_at"`
}

// IsAllowed returns true when virtualModelName passes the allow/deny prefix
// rules defined on this access grant.
//
// Evaluation order (mirrors the exposure RuleEngine):
//  1. Denied prefixes checked first — if any matches, return false.
//  2. If AllowedPrefixes is empty, return true (allow all).
//  3. If any allowed prefix matches, return true.
//  4. Default: false.
//
// Pattern matching uses the same path.Match glob as catalog exposure rules
// (defined in rules.go — this file calls globMatch from that file).
func (a *ProjectProviderAccess) IsAllowed(virtualModelName string) bool {
	// 1. Deny wins.
	for _, pat := range a.DeniedPrefixes {
		if globMatch(pat, virtualModelName) {
			return false
		}
	}
	// 2. Empty allow list = allow everything not denied.
	if len(a.AllowedPrefixes) == 0 {
		return true
	}
	// 3. Check allow patterns.
	for _, pat := range a.AllowedPrefixes {
		if globMatch(pat, virtualModelName) {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// ProjectProviderAccessStore
// ─────────────────────────────────────────────────────────────────────────────

// ProjectProviderAccessStore handles DB access for project_provider_access.
type ProjectProviderAccessStore struct {
	db *sqlx.DB
}

// NewProjectProviderAccessStore constructs a store.
func NewProjectProviderAccessStore(db *sqlx.DB) *ProjectProviderAccessStore {
	return &ProjectProviderAccessStore{db: db}
}

// projectProviderAccessRow is the internal DB scan target; using pq.StringArray
// for PostgreSQL TEXT[] columns.
type projectProviderAccessRow struct {
	ID              string         `db:"id"`
	ProjectID       string         `db:"project_id"`
	ProviderID      string         `db:"provider_id"`
	ProviderName    string         `db:"provider_name"`
	ExposureMode    string         `db:"exposure_mode"`
	AllowedPrefixes pq.StringArray `db:"allowed_prefixes"`
	DeniedPrefixes  pq.StringArray `db:"denied_prefixes"`
	Enabled         bool           `db:"enabled"`
	CreatedAt       time.Time      `db:"created_at"`
	UpdatedAt       time.Time      `db:"updated_at"`
}

func (r *projectProviderAccessRow) toAccess() ProjectProviderAccess {
	return ProjectProviderAccess{
		ID: r.ID, ProjectID: r.ProjectID, ProviderID: r.ProviderID,
		ProviderName: r.ProviderName, ExposureMode: r.ExposureMode,
		AllowedPrefixes: []string(r.AllowedPrefixes),
		DeniedPrefixes:  []string(r.DeniedPrefixes),
		Enabled: r.Enabled, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

const projectProviderAccessCols = `
	ppa.id::text, ppa.project_id::text, ppa.provider_id::text,
	p.name AS provider_name,
	COALESCE(p.exposure_mode,'managed') AS exposure_mode,
	COALESCE(ppa.allowed_prefixes,'{}') AS allowed_prefixes,
	COALESCE(ppa.denied_prefixes,'{}') AS denied_prefixes,
	ppa.enabled, ppa.created_at, ppa.updated_at`

// ListForProject returns all enabled access grants for a project.
func (s *ProjectProviderAccessStore) ListForProject(ctx context.Context, projectID string) ([]ProjectProviderAccess, error) {
	var rows []projectProviderAccessRow
	err := s.db.SelectContext(ctx, &rows, `
		SELECT `+projectProviderAccessCols+`
		FROM project_provider_access ppa
		JOIN providers p ON p.id = ppa.provider_id
		WHERE ppa.project_id::text = $1 AND ppa.enabled = TRUE
		  AND p.enabled = TRUE
		ORDER BY p.name`, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]ProjectProviderAccess, len(rows))
	for i := range rows {
		out[i] = rows[i].toAccess()
	}
	return out, nil
}

// ListAll returns all enabled grants across all projects for catalog/hybrid
// providers. Used at gateway startup and the 60s reload to seed Redis.
func (s *ProjectProviderAccessStore) ListAll(ctx context.Context) ([]ProjectProviderAccess, error) {
	var rows []projectProviderAccessRow
	err := s.db.SelectContext(ctx, &rows, `
		SELECT `+projectProviderAccessCols+`
		FROM project_provider_access ppa
		JOIN providers p ON p.id = ppa.provider_id
		WHERE ppa.enabled = TRUE AND p.enabled = TRUE
		  AND p.exposure_mode IN ('catalog','hybrid')
		ORDER BY ppa.project_id, p.name`)
	if err != nil {
		return nil, err
	}
	out := make([]ProjectProviderAccess, len(rows))
	for i := range rows {
		out[i] = rows[i].toAccess()
	}
	return out, nil
}
