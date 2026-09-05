package handlers

// credential.go — Admin REST API for the provider_credentials pool
// (migration 062). Complements project_provider_access CRUD in catalog.go:
// this file manages the credential POOL itself (create/list/enable/disable/
// delete a named credential under a provider); catalog.go's
// Grant/UpdateProjectProviderAccess manage which project is PINNED to which
// credential from that pool.
//
// Security invariant enforced throughout this file: no handler here ever
// selects, marshals, or logs provider_credentials.secret_ciphertext or its
// decrypted form. catalog.ProviderCredential (the struct every response is
// built from) has no field for it — the only place the plaintext secret ever
// exists in memory is inside credential_resolver.go, for the duration of one
// outbound request.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/nexusllm/nexusllm/internal/catalog"
	"go.uber.org/zap"
)

// CreateProviderCredential handles POST /admin/v1/providers/:provider_id/credentials
//
// Request body:
//
//	{
//	  "name":           "production-app-a",
//	  "secret":         "sk-or-v1-...",     // plaintext; encrypted before storage, never echoed back
//	  "api_key_header": "Authorization",     // optional; defaults to the provider's header
//	  "is_default":     false                // optional; at most one default per provider
//	}
//
// The response never contains the secret — see catalog.ProviderCredential.
func (h *CatalogHandler) CreateProviderCredential(c *gin.Context) {
	providerID := c.Param("id") // nested under /providers/:id/credentials...
	var in struct {
		Name         string          `json:"name" binding:"required"`
		Secret       string          `json:"secret" binding:"required"`
		APIKeyHeader *string         `json:"api_key_header"`
		IsDefault    bool            `json:"is_default"`
		Metadata     json.RawMessage `json:"metadata"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if _, err := h.store.Get(c.Request.Context(), providerID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider not found: " + providerID})
		return
	}

	if h.secrets == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "credential encryption is not configured on this server " +
				"(NEXUS_CREDENTIAL_ENCRYPTION_KEY is unset) — refusing to create a credential",
		})
		return
	}
	ciphertext, err := h.secrets.Encrypt(in.Secret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt credential"})
		return
	}

	created, err := h.credStore.Create(c.Request.Context(), catalog.CreateInput{
		ProviderID:   providerID,
		Name:         in.Name,
		APIKeyHeader: in.APIKeyHeader,
		IsDefault:    in.IsDefault,
		Metadata:     in.Metadata,
	}, ciphertext)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "DB error: " + err.Error()})
		return
	}

	h.log.Info("provider credential created",
		zap.String("provider_id", providerID),
		zap.String("credential_id", created.ID),
		zap.String("name", created.Name),
		zap.Bool("is_default", created.IsDefault),
	)
	c.JSON(http.StatusCreated, created)
}

// ListProviderCredentials handles GET /admin/v1/providers/:provider_id/credentials
//
// Never returns secret material — assigned_count shows how many projects are
// currently pinned to each credential, so an operator can see the blast
// radius before disabling or deleting one.
func (h *CatalogHandler) ListProviderCredentials(c *gin.Context) {
	providerID := c.Param("id") // nested under /providers/:id/credentials...
	creds, err := h.credStore.List(c.Request.Context(), providerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": creds, "total": len(creds), "provider_id": providerID})
}

// UpdateProviderCredential handles PATCH /admin/v1/providers/:provider_id/credentials/:credential_id
//
// Supports enabling/disabling (rotation/incident response) and changing which
// credential is the provider's default. Renaming or replacing the secret is
// intentionally NOT supported here — rotate by creating a new named
// credential and re-pointing project_provider_access.credential_id at it
// (see migrations/062_provider_credentials.sql header for the rotation
// rationale: an in-flight request already carries its resolved secret in
// memory, so rotating the DB row never disrupts requests in progress; only
// the next request re-resolves and picks up the change).
func (h *CatalogHandler) UpdateProviderCredential(c *gin.Context) {
	providerID := c.Param("id") // nested under /providers/:id/credentials...
	credentialID := c.Param("credential_id")
	var in struct {
		Enabled   *bool `json:"enabled"`
		IsDefault *bool `json:"is_default"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	existing, err := h.credStore.Get(c.Request.Context(), credentialID)
	if err != nil || existing.ProviderID != providerID {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found for this provider: " + credentialID})
		return
	}

	if in.Enabled != nil {
		if err := h.credStore.SetEnabled(c.Request.Context(), credentialID, *in.Enabled); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		h.log.Info("provider credential enabled state changed",
			zap.String("credential_id", credentialID), zap.Bool("enabled", *in.Enabled))
	}
	if in.IsDefault != nil && *in.IsDefault {
		if err := setDefaultCredential(c.Request.Context(), h, providerID, credentialID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		h.log.Info("provider default credential changed",
			zap.String("provider_id", providerID), zap.String("credential_id", credentialID))
	}

	updated, err := h.credStore.Get(c.Request.Context(), credentialID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, updated)
}

// setDefaultCredential clears any existing default for providerID and marks
// credentialID as the new one, atomically.
func setDefaultCredential(ctx context.Context, h *CatalogHandler, providerID, credentialID string) error {
	tx, err := h.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE provider_credentials SET is_default = FALSE, updated_at = NOW()
		 WHERE provider_id::text = $1 AND is_default = TRUE`, providerID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE provider_credentials SET is_default = TRUE, updated_at = NOW()
		 WHERE id::text = $1`, credentialID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteProviderCredential handles DELETE /admin/v1/providers/:provider_id/credentials/:credential_id
//
// Any project_provider_access rows pinned to this credential have
// credential_id set to NULL by the DB (ON DELETE SET NULL) and fall back to
// the provider's default credential on their next request — this is a real
// access change (see migrations/062), so the response says so explicitly
// rather than silently succeeding.
func (h *CatalogHandler) DeleteProviderCredential(c *gin.Context) {
	providerID := c.Param("id") // nested under /providers/:id/credentials...
	credentialID := c.Param("credential_id")

	existing, err := h.credStore.Get(c.Request.Context(), credentialID)
	if err != nil || existing.ProviderID != providerID {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found for this provider: " + credentialID})
		return
	}
	if err := h.credStore.Delete(c.Request.Context(), credentialID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.log.Warn("provider credential deleted",
		zap.String("provider_id", providerID),
		zap.String("credential_id", credentialID),
		zap.Int("projects_reassigned_to_default", existing.AssignedCount),
	)

	note := "no projects were pinned to this credential"
	if existing.AssignedCount > 0 {
		note = "this credential was pinned by " + strconv.Itoa(existing.AssignedCount) +
			" project(s) — they now fall back to the provider's default credential"
	}
	c.JSON(http.StatusOK, gin.H{"message": "credential deleted", "note": note})
}
