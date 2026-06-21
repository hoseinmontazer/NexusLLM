package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	internalauth "github.com/nexusllm/nexusllm/internal/auth"
	"github.com/nexusllm/nexusllm/internal/models"
	"github.com/redis/go-redis/v9"
)

// APIKeyHandler manages API key lifecycle.
type APIKeyHandler struct {
	db  *sqlx.DB
	rdb *redis.Client
}

// NewAPIKeyHandler constructs an APIKeyHandler.
func NewAPIKeyHandler(db *sqlx.DB, rdb *redis.Client) *APIKeyHandler {
	return &APIKeyHandler{db: db, rdb: rdb}
}

// CreateAPIKey handles POST /admin/v1/teams/:id/api-keys
// The raw key is returned exactly once — it is never stored in plaintext.
func (h *APIKeyHandler) CreateAPIKey(c *gin.Context) {
	teamID := c.Param("id")

	var input struct {
		Name      string     `json:"name"       binding:"required"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Verify team exists
	var exists bool
	if err := h.db.GetContext(c.Request.Context(), &exists,
		`SELECT EXISTS(SELECT 1 FROM teams WHERE id = $1 AND active = TRUE)`, teamID); err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "team not found"})
		return
	}

	raw, hash, err := internalauth.GenerateAPIKey()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate key"})
		return
	}

	keyID := uuid.New().String()
	prefix := raw[:12] // "nxs_" + 8 chars

	apiKey := models.APIKey{
		ID:        keyID,
		TeamID:    teamID,
		Name:      input.Name,
		KeyHash:   hash,
		KeyPrefix: prefix,
		Active:    true,
		ExpiresAt: input.ExpiresAt,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	_, err = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO api_keys (id, team_id, name, key_hash, key_prefix, active, expires_at, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		apiKey.ID, apiKey.TeamID, apiKey.Name, apiKey.KeyHash, apiKey.KeyPrefix,
		apiKey.Active, apiKey.ExpiresAt, apiKey.CreatedAt, apiKey.UpdatedAt,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store key"})
		return
	}

	// Return key once
	c.JSON(http.StatusCreated, gin.H{
		"id":         apiKey.ID,
		"name":       apiKey.Name,
		"key":        raw, // shown once only
		"key_prefix": apiKey.KeyPrefix,
		"team_id":    apiKey.TeamID,
		"expires_at": apiKey.ExpiresAt,
		"created_at": apiKey.CreatedAt,
	})
}

// ListAPIKeys handles GET /admin/v1/teams/:id/api-keys
func (h *APIKeyHandler) ListAPIKeys(c *gin.Context) {
	teamID := c.Param("id")
	keys := []models.APIKey{}
	if err := h.db.SelectContext(c.Request.Context(), &keys,
		`SELECT id, team_id, name, key_prefix, active, last_used_at, expires_at, created_at, updated_at
		 FROM api_keys WHERE team_id = $1 ORDER BY created_at DESC`, teamID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	if keys == nil {
		keys = []models.APIKey{}
	}
	c.JSON(http.StatusOK, gin.H{"data": keys, "total": len(keys)})
}

// RevokeAPIKey handles DELETE /admin/v1/api-keys/:id
func (h *APIKeyHandler) RevokeAPIKey(c *gin.Context) {
	keyID := c.Param("id")

	// Get the hash before deactivating (needed for cache invalidation)
	var keyHash string
	if err := h.db.GetContext(c.Request.Context(), &keyHash,
		`SELECT key_hash FROM api_keys WHERE id = $1`, keyID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
		return
	}

	res, err := h.db.ExecContext(c.Request.Context(),
		`UPDATE api_keys SET active = FALSE, updated_at = NOW() WHERE id = $1`, keyID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
		return
	}

	// Purge from Redis cache
	_ = h.rdb.Del(c.Request.Context(), "nexus:apikey:"+keyHash)

	c.JSON(http.StatusOK, gin.H{"message": "api key revoked"})
}
