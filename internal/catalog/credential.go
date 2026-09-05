package catalog

// credential.go — Layer-1c: multi-credential pool per provider.
//
// providers.api_key (provider.go) is a single, globally-shared credential per
// provider row. provider_credentials (migration 062) extends that into a
// named pool: a provider can hold many credentials, and project_provider_access
// (provider_access.go) optionally pins one specific credential to a project's
// grant. See internal/catalog/credential_resolver.go for the selection
// algorithm that ties these together.
//
// Secrets are always encrypted at rest (internal/secretstore) and this file
// never returns secret_ciphertext from a List/Get call used by admin
// endpoints — only credential_resolver.go's internal decrypt path reads it.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jmoiron/sqlx"
)

// ProviderCredential is one row from provider_credentials, WITHOUT the secret.
// This is the shape returned by every admin-facing List/Get call.
type ProviderCredential struct {
	ID            string          `db:"id" json:"id"`
	ProviderID    string          `db:"provider_id" json:"provider_id"`
	Name          string          `db:"name" json:"name"`
	APIKeyHeader  *string         `db:"api_key_header" json:"api_key_header,omitempty"`
	IsDefault     bool            `db:"is_default" json:"is_default"`
	Enabled       bool            `db:"enabled" json:"enabled"`
	Metadata      json.RawMessage `db:"metadata" json:"metadata,omitempty"`
	LastUsedAt    *time.Time      `db:"last_used_at" json:"last_used_at,omitempty"`
	CreatedAt     time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time       `db:"updated_at" json:"updated_at"`
	AssignedCount int             `db:"assigned_count" json:"assigned_count"`
}

// ProviderCredentialStore handles DB read/write for provider_credentials.
// It never exposes secret_ciphertext through any exported method except
// getSecretCiphertext, which is unexported and used only by CredentialResolver
// in this same package.
type ProviderCredentialStore struct {
	db *sqlx.DB
}

// NewProviderCredentialStore constructs a store.
func NewProviderCredentialStore(db *sqlx.DB) *ProviderCredentialStore {
	return &ProviderCredentialStore{db: db}
}

const credentialListCols = `
	c.id::text, c.provider_id::text, c.name, c.api_key_header,
	c.is_default, c.enabled, c.metadata, c.last_used_at, c.created_at, c.updated_at,
	COALESCE((SELECT COUNT(*) FROM project_provider_access ppa
	          WHERE ppa.credential_id = c.id AND ppa.enabled = TRUE), 0) AS assigned_count`

// List returns every credential for a provider, ordered by name. Never
// includes secret_ciphertext.
func (s *ProviderCredentialStore) List(ctx context.Context, providerID string) ([]ProviderCredential, error) {
	var rows []ProviderCredential
	err := s.db.SelectContext(ctx, &rows, `
		SELECT `+credentialListCols+`
		FROM provider_credentials c
		WHERE c.provider_id::text = $1
		ORDER BY c.name`, providerID)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []ProviderCredential{}
	}
	return rows, nil
}

// Get returns a single credential by ID (no secret). Returns sql.ErrNoRows if
// not found.
func (s *ProviderCredentialStore) Get(ctx context.Context, id string) (*ProviderCredential, error) {
	var c ProviderCredential
	err := s.db.GetContext(ctx, &c, `
		SELECT `+credentialListCols+`
		FROM provider_credentials c
		WHERE c.id::text = $1`, id)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// CreateInput is the payload for Create — Secret is the PLAINTEXT credential;
// it is encrypted before being written and is never logged or returned.
type CreateInput struct {
	ProviderID   string
	Name         string
	Secret       string
	APIKeyHeader *string
	IsDefault    bool
	Metadata     json.RawMessage
}

// Create encrypts secret and inserts a new provider_credentials row.
// If isDefault is true, any existing default for this provider is cleared
// first (there can be at most one default per provider — enforced by both
// this transaction and a partial unique index as a second line of defense).
func (s *ProviderCredentialStore) Create(ctx context.Context, in CreateInput, secretCiphertext string) (*ProviderCredential, error) {
	meta := in.Metadata
	if meta == nil {
		meta = json.RawMessage(`{}`)
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if in.IsDefault {
		if _, err := tx.ExecContext(ctx,
			`UPDATE provider_credentials SET is_default = FALSE, updated_at = NOW()
			 WHERE provider_id::text = $1 AND is_default = TRUE`, in.ProviderID); err != nil {
			return nil, err
		}
	}

	var id string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO provider_credentials
			(provider_id, name, secret_ciphertext, api_key_header, is_default, enabled, metadata)
		VALUES ($1, $2, $3, $4, $5, TRUE, $6)
		RETURNING id::text`,
		in.ProviderID, in.Name, secretCiphertext, in.APIKeyHeader, in.IsDefault, []byte(meta),
	).Scan(&id)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// SetEnabled toggles a credential on/off. Disabling a credential that is the
// provider's default, or that a project is pinned to, does NOT reassign
// anything — resolution fails closed (provider_credential_unavailable) rather
// than silently switching to a different credential. This is deliberate: see
// credential_resolver.go.
func (s *ProviderCredentialStore) SetEnabled(ctx context.Context, id string, enabled bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE provider_credentials SET enabled = $2, updated_at = NOW() WHERE id::text = $1`,
		id, enabled)
	return err
}

// Delete permanently removes a credential. project_provider_access rows
// pinned to it have credential_id set to NULL (ON DELETE SET NULL, migration
// 062) rather than being deleted — the project keeps its provider
// authorization but falls back to the provider default, which the caller
// should surface clearly (this is a real access change, not a no-op).
func (s *ProviderCredentialStore) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM provider_credentials WHERE id::text = $1`, id)
	return err
}

// touchLastUsed is best-effort — failures are not propagated to the request
// path. Called by CredentialResolver after a successful resolution.
func (s *ProviderCredentialStore) touchLastUsed(ctx context.Context, id string) {
	_, _ = s.db.ExecContext(ctx,
		`UPDATE provider_credentials SET last_used_at = NOW() WHERE id::text = $1`, id)
}
