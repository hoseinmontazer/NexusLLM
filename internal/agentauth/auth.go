// Package agentauth handles authentication for node agents.
// Each node receives a JWT token on first registration.
// The agent presents it as: Authorization: Bearer <token>
// The control plane validates it and extracts the node_id claim.
package agentauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

const (
	tokenTTL        = 365 * 24 * time.Hour // 1 year — rotated on re-registration
	claimNodeID     = "node_id"
	claimNodeHost   = "node_host"
	issuer          = "nexus-control-plane"
)

// NodeClaims are the JWT claims embedded in an agent token.
type NodeClaims struct {
	NodeID   string `json:"node_id"`
	Hostname string `json:"hostname"`
	jwt.RegisteredClaims
}

// Service handles node token issuance and validation.
type Service struct {
	db     *sqlx.DB
	secret []byte
}

// NewService constructs an agentauth.Service.
func NewService(db *sqlx.DB, jwtSecret string) *Service {
	return &Service{db: db, secret: []byte(jwtSecret)}
}

// IssueToken creates a signed JWT for a node and stores its hash in node_tokens.
// Returns the raw token string (caller must deliver it to the agent once).
func (s *Service) IssueToken(ctx context.Context, nodeID, hostname string) (string, error) {
	now := time.Now()
	claims := NodeClaims{
		NodeID:   nodeID,
		Hostname: hostname,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   nodeID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(tokenTTL)),
			ID:        uuid.New().String(),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}

	hash := hashToken(signed)

	// Upsert — replace any existing token for this node
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO node_tokens (id, node_id, token_hash, issued_at, expires_at, revoked)
		VALUES ($1, $2, $3, $4, $5, FALSE)
		ON CONFLICT (node_id) DO UPDATE
		  SET token_hash = EXCLUDED.token_hash,
		      issued_at  = EXCLUDED.issued_at,
		      expires_at = EXCLUDED.expires_at,
		      revoked    = FALSE`,
		uuid.New().String(), nodeID, hash, now, now.Add(tokenTTL),
	)
	if err != nil {
		return "", fmt.Errorf("store token: %w", err)
	}

	return signed, nil
}

// Validate parses and validates an agent token. Returns NodeClaims on success.
// Checks: signature, expiry, not-revoked (DB lookup).
func (s *Service) Validate(ctx context.Context, rawToken string) (*NodeClaims, error) {
	// 1. Parse + verify signature and expiry
	token, err := jwt.ParseWithClaims(rawToken, &NodeClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.secret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	claims, ok := token.Claims.(*NodeClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	// 2. Check DB — token must be stored and not revoked
	hash := hashToken(rawToken)
	var revoked bool
	err = s.db.QueryRowContext(ctx,
		`SELECT revoked FROM node_tokens WHERE token_hash = $1`, hash,
	).Scan(&revoked)
	if err != nil {
		return nil, fmt.Errorf("token not found in registry")
	}
	if revoked {
		return nil, fmt.Errorf("token revoked")
	}

	return claims, nil
}

// RevokeNode revokes the token for a node (e.g. on decommission).
func (s *Service) RevokeNode(ctx context.Context, nodeID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE node_tokens SET revoked = TRUE WHERE node_id = $1`, nodeID)
	return err
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
