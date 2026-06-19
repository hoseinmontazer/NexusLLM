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

// TeamClaims holds identity and policy metadata extracted from an API key or JWT.
type TeamClaims struct {
	OrgID        string   `json:"org_id"`
	TeamID       string   `json:"team_id"`
	TeamName     string   `json:"team_name"`
	TeamPriority int      `json:"team_priority"`
	Permissions  []string `json:"permissions"`
}

// jwtClaims wraps TeamClaims for use inside a JWT.
type jwtClaims struct {
	TeamClaims
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
// It returns the raw (plaintext) key and its SHA-256 hash.
func GenerateAPIKey() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate api key: %w", err)
	}
	raw = "nxs_" + hex.EncodeToString(b)
	hash = HashAPIKey(raw)
	return raw, hash, nil
}

// ValidateAPIKey looks up the SHA-256 hash of key first in Redis, then in PostgreSQL.
// On a DB hit it populates the Redis cache for future requests.
func (s *Service) ValidateAPIKey(ctx context.Context, key string) (*TeamClaims, error) {
	if !strings.HasPrefix(key, "nxs_") {
		return nil, errors.New("invalid api key format")
	}

	hash := HashAPIKey(key)
	cacheKey := apiKeyCachePrefix + hash

	// 1. Redis cache lookup
	cached, err := s.rdb.Get(ctx, cacheKey).Bytes()
	if err == nil {
		var claims TeamClaims
		if jsonErr := json.Unmarshal(cached, &claims); jsonErr == nil {
			return &claims, nil
		}
	}

	// 2. PostgreSQL fallback
	query := `
		SELECT
			o.id          AS org_id,
			t.id          AS team_id,
			t.name        AS team_name,
			t.priority    AS team_priority
		FROM api_keys ak
		JOIN teams   t ON t.id = ak.team_id
		JOIN organizations o ON o.id = t.org_id
		WHERE ak.key_hash = $1
		  AND ak.active   = TRUE
		  AND t.active    = TRUE
		  AND o.active    = TRUE
		  AND (ak.expires_at IS NULL OR ak.expires_at > NOW())
	`

	type row struct {
		OrgID        string `db:"org_id"`
		TeamID       string `db:"team_id"`
		TeamName     string `db:"team_name"`
		TeamPriority int    `db:"team_priority"`
	}

	var r row
	if err := s.db.GetContext(ctx, &r, query, hash); err != nil {
		return nil, fmt.Errorf("api key not found or inactive: %w", err)
	}

	// Load permissions from DB
	perms, err := s.loadPermissions(ctx, r.TeamID)
	if err != nil {
		return nil, fmt.Errorf("load permissions: %w", err)
	}

	claims := &TeamClaims{
		OrgID:        r.OrgID,
		TeamID:       r.TeamID,
		TeamName:     r.TeamName,
		TeamPriority: r.TeamPriority,
		Permissions:  perms,
	}

	// Update last-used timestamp (best-effort, non-blocking)
	go func() {
		bgCtx := context.Background()
		_, _ = s.db.ExecContext(bgCtx,
			"UPDATE api_keys SET last_used_at = NOW() WHERE key_hash = $1", hash)
	}()

	// Populate Redis cache
	if data, marshalErr := json.Marshal(claims); marshalErr == nil {
		_ = s.rdb.Set(ctx, cacheKey, data, s.cacheTTL).Err()
	}

	return claims, nil
}

// ValidateJWT parses and validates a JWT string, returning the embedded TeamClaims.
func (s *Service) ValidateJWT(ctx context.Context, tokenStr string) (*TeamClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &jwtClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.jwtSecret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("invalid jwt: %w", err)
	}

	claims, ok := token.Claims.(*jwtClaims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid jwt claims")
	}

	tc := &claims.TeamClaims
	return tc, nil
}

// IssueJWT creates a signed JWT embedding the given TeamClaims with a 24 h expiry.
func (s *Service) IssueJWT(claims *TeamClaims, ttl time.Duration) (string, error) {
	jc := jwtClaims{
		TeamClaims: *claims,
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

// loadPermissions fetches the list of allowed model names for a team.
func (s *Service) loadPermissions(ctx context.Context, teamID string) ([]string, error) {
	var models []string
	query := `
		SELECT m.name
		FROM team_model_permissions tmp
		JOIN models m ON m.id = tmp.model_id
		WHERE tmp.team_id = $1 AND m.active = TRUE
	`
	if err := s.db.SelectContext(ctx, &models, query, teamID); err != nil {
		return nil, err
	}
	return models, nil
}
