// Package auth provides API key validation, JWT issuance, and request identity.
//
// Domain model (Organisation as root):
//
//	Organization
//	  └── Project          ← execution root (rate limits, priority, quota)
//	  └── Team (optional)  ← RBAC/membership only
//
// RequestClaims carries only stable identifiers. No mutable display strings
// participate in runtime decisions. TeamID is kept for RBAC/ACL lookups that
// are still team-scoped (model permissions, prompt policies) but it must never
// drive scheduling, rate limiting, or quota decisions.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
)

const (
	apiKeyCachePrefix = "nexus:apikey:"
	apiKeyCacheTTL    = 5 * time.Minute
)

// RequestClaims is the canonical identity context attached to every request
// after authentication. It contains only stable, immutable identifiers.
//
// Execution root:  OrgID → ProjectID
// RBAC/ACL root:   TeamID (optional — empty when key is project-scoped only)
// Display-only:    TeamName, ProjectName  (never used in runtime decisions)
type RequestClaims struct {
	// Stable identifiers — the only fields that may drive runtime logic.
	UserID    string `json:"user_id,omitempty"`
	OrgID     string `json:"org_id"`
	ProjectID string `json:"project_id,omitempty"`
	APIKeyID  string `json:"api_key_id,omitempty"`
	Role      string `json:"role,omitempty"`
	Email     string `json:"email,omitempty"`

	// RBAC/membership — used only for model ACL and prompt-policy lookups.
	// Must never be used for rate limiting, scheduling, or quota evaluation.
	TeamID string `json:"team_id,omitempty"`

	// Scheduling weight — derived from the project, not the team.
	ProjectPriorityWeight int `json:"project_priority_weight,omitempty"`

	// Model permissions resolved at auth time (model names this key may use).
	Permissions []string `json:"permissions"`

	// Display-only — do not use in any runtime decision.
	TeamName    string `json:"team_name,omitempty"`    // display only
	ProjectName string `json:"project_name,omitempty"` // display only
}

// TeamClaims is a backward-compatibility alias for RequestClaims.
// New code should use RequestClaims directly.
type TeamClaims = RequestClaims

// jwtClaims wraps RequestClaims for JWT signing.
type jwtClaims struct {
	RequestClaims
	jwt.RegisteredClaims
}

// Service provides authentication helpers.
type Service struct {
	rdb       *redis.Client
	db        *sqlx.DB
	jwtSecret []byte
	cacheTTL  time.Duration
}

// NewService constructs an auth Service.
func NewService(rdb *redis.Client, db *sqlx.DB, jwtSecret string, cacheTTL time.Duration) *Service {
	return &Service{
		rdb:       rdb,
		db:        db,
		jwtSecret: []byte(jwtSecret),
		cacheTTL:  cacheTTL,
	}
}

// HashAPIKey returns the hex-encoded SHA-256 of key.
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// GenerateAPIKey creates a cryptographically random API key with "nxs_" prefix.
// Returns the raw (plaintext) key and its SHA-256 hash.
func GenerateAPIKey() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate api key: %w", err)
	}
	raw = "nxs_" + hex.EncodeToString(b)
	hash = HashAPIKey(raw)
	return raw, hash, nil
}

// ValidateAPIKey looks up the SHA-256 hash first in Redis, then in PostgreSQL.
// On a DB hit it populates the Redis cache for future requests.
//
// Resolution order for project context:
//  1. api_keys.project_id  — key is directly scoped to a project
//  2. No project context  — legacy team-only key (TeamID set, ProjectID empty)
//
// The org is always resolved via the team → org JOIN so that the org-disabled
// governance check always has a valid OrgID.
func (s *Service) ValidateAPIKey(ctx context.Context, key string) (*RequestClaims, error) {
	if !strings.HasPrefix(key, "nxs_") {
		return nil, errors.New("invalid api key format")
	}

	hash := HashAPIKey(key)
	cacheKey := apiKeyCachePrefix + hash

	// 1. Redis cache — fast path
	if cached, err := s.rdb.Get(ctx, cacheKey).Bytes(); err == nil {
		var claims RequestClaims
		if jsonErr := json.Unmarshal(cached, &claims); jsonErr == nil {
			return &claims, nil
		}
	}

	// 2. PostgreSQL — resolve org via team membership
	// org_id is the execution root; team_id is kept for RBAC only.
	const query = `
		SELECT
			ak.id                                                  AS api_key_id,
			o.id                                                   AS org_id,
			t.id                                                   AS team_id,
			t.name                                                 AS team_name,
			COALESCE(ak.project_id::text, '')                     AS project_id,
			COALESCE(ak.project_name,     '')                     AS project_name,
			COALESCE(ak.project_priority_weight, 500)             AS project_priority_weight
		FROM api_keys ak
		JOIN teams        t ON t.id  = ak.team_id
		JOIN organizations o ON o.id = t.org_id
		WHERE ak.key_hash = $1
		  AND ak.active   = TRUE
		  AND t.active    = TRUE
		  AND o.active    = TRUE
		  AND (ak.expires_at IS NULL OR ak.expires_at > NOW())
	`
	type row struct {
		APIKeyID              string `db:"api_key_id"`
		OrgID                 string `db:"org_id"`
		TeamID                string `db:"team_id"`
		TeamName              string `db:"team_name"`
		ProjectID             string `db:"project_id"`
		ProjectName           string `db:"project_name"`
		ProjectPriorityWeight int    `db:"project_priority_weight"`
	}
	var r row
	if err := s.db.GetContext(ctx, &r, query, hash); err != nil {
		// Fallback: pre-022 schema without project columns
		if isColumnMissing(err) {
			return s.validateAPIKeyLegacy(ctx, hash)
		}
		return nil, fmt.Errorf("api key not found or inactive: %w", err)
	}

	// Load model permissions (team-scoped ACL; empty for project-only keys)
	perms, err := s.loadPermissions(ctx, r.TeamID)
	if err != nil {
		return nil, fmt.Errorf("load permissions: %w", err)
	}

	claims := &RequestClaims{
		APIKeyID:              r.APIKeyID,
		OrgID:                 r.OrgID,
		TeamID:                r.TeamID,
		TeamName:              r.TeamName,
		ProjectID:             r.ProjectID,
		ProjectName:           r.ProjectName,
		ProjectPriorityWeight: r.ProjectPriorityWeight,
		Permissions:           perms,
	}

	// Update last-used timestamp (best-effort, non-blocking)
	go func() {
		bgCtx := context.Background()
		_, _ = s.db.ExecContext(bgCtx,
			"UPDATE api_keys SET last_used_at = NOW() WHERE key_hash = $1", hash)
	}()

	// Cache the resolved claims
	if data, marshalErr := json.Marshal(claims); marshalErr == nil {
		_ = s.rdb.Set(ctx, cacheKey, data, s.cacheTTL).Err()
	}

	return claims, nil
}

// ValidateJWT parses and validates a JWT, returning the embedded RequestClaims.
func (s *Service) ValidateJWT(ctx context.Context, tokenStr string) (*RequestClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &jwtClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.jwtSecret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("invalid jwt: %w", err)
	}
	c, ok := token.Claims.(*jwtClaims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid jwt claims")
	}
	tc := &c.RequestClaims
	return tc, nil
}

// IssueJWT creates a signed JWT embedding the given RequestClaims.
func (s *Service) IssueJWT(claims *RequestClaims, ttl time.Duration) (string, error) {
	jc := jwtClaims{
		RequestClaims: *claims,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			Issuer:    "nexusllm",
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jc)
	return tok.SignedString(s.jwtSecret)
}

// InvalidateAPIKeyCache removes a cached API key entry from Redis.
func (s *Service) InvalidateAPIKeyCache(ctx context.Context, keyHash string) error {
	return s.rdb.Del(ctx, apiKeyCachePrefix+keyHash).Err()
}

// loadPermissions fetches the list of allowed model names for a team (RBAC).
//
// Unified authorization: every Public Model — local runtime or remote provider —
// must have a row in the models table and an explicit grant in team_model_permissions
// before any API key belonging to this team can use it. Backend type is irrelevant.
//
// This is a single query. There is no second permission table for remote or
// virtual models. Any model that can be called MUST be registered in models
// (via RegisterExternalModel, RegisterCatalogAlias, or DeployModel) and granted
// via team_model_permissions. Mode-B virtual catalog models are a discovery
// feature only — they are not callable until promoted to a models row.
func (s *Service) loadPermissions(ctx context.Context, teamID string) ([]string, error) {
	if teamID == "" {
		return []string{}, nil
	}
	var names []string
	err := s.db.SelectContext(ctx, &names, `
		SELECT m.name
		FROM team_model_permissions tmp
		JOIN models m ON m.id = tmp.model_id
		WHERE tmp.team_id = $1 AND m.enabled = TRUE`, teamID)
	return names, err
}

// validateAPIKeyLegacy handles the pre-migration-022 schema (no project columns).
func (s *Service) validateAPIKeyLegacy(ctx context.Context, hash string) (*RequestClaims, error) {
	type row struct {
		APIKeyID string `db:"api_key_id"`
		OrgID    string `db:"org_id"`
		TeamID   string `db:"team_id"`
		TeamName string `db:"team_name"`
	}
	var r row
	if err := s.db.GetContext(ctx, &r, `
		SELECT ak.id AS api_key_id,
		       o.id  AS org_id,
		       t.id  AS team_id,
		       t.name AS team_name
		FROM api_keys ak
		JOIN teams t        ON t.id  = ak.team_id
		JOIN organizations o ON o.id = t.org_id
		WHERE ak.key_hash = $1
		  AND ak.active = TRUE AND t.active = TRUE AND o.active = TRUE
		  AND (ak.expires_at IS NULL OR ak.expires_at > NOW())`, hash); err != nil {
		return nil, fmt.Errorf("api key not found or inactive: %w", err)
	}
	perms, err := s.loadPermissions(ctx, r.TeamID)
	if err != nil {
		return nil, fmt.Errorf("load permissions: %w", err)
	}
	claims := &RequestClaims{
		APIKeyID:    r.APIKeyID,
		OrgID:       r.OrgID,
		TeamID:      r.TeamID,
		TeamName:    r.TeamName,
		Permissions: perms,
	}
	if data, err := json.Marshal(claims); err == nil {
		_ = s.rdb.Set(ctx, apiKeyCachePrefix+hash, data, s.cacheTTL).Err()
	}
	return claims, nil
}

// isColumnMissing returns true when the Postgres error indicates a missing column (42703).
func isColumnMissing(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "project_id") ||
		strings.Contains(s, "project_name") ||
		strings.Contains(s, "project_priority_weight") ||
		strings.Contains(s, "api_key_id") ||
		strings.Contains(s, "42703")
}
