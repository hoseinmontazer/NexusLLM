package catalog

// credential_resolver.go — the single, provider-agnostic component that
// decides WHICH upstream credential handles a request. Every call site that
// needs to put a real "Authorization: Bearer <token>" on an outbound provider
// request MUST go through Resolve here — never re-implement credential
// selection locally (see internal/proxy/handler.go, multiservice.go,
// internal/admin/handlers/catalog.go for the wired call sites).
//
// Resolution is deterministic and fails closed (see ErrCredentialUnavailable):
// a project that is explicitly pinned to a credential never silently falls
// back to a different one just because the pinned credential is disabled.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/secretstore"
)

// ErrCredentialUnavailable is returned when no usable credential can be
// resolved for a (provider, project) pair. Callers must map this to a
// distinct, stable error code ("provider_credential_unavailable") in the
// HTTP response — never fall back to a different credential on this error.
var ErrCredentialUnavailable = errors.New("provider_credential_unavailable")

// ResolvedCredential is the outcome of a successful resolution. Secret is the
// decrypted plaintext — callers must use it immediately to build the outbound
// request and must never log it, put it in a metric label, or return it in
// any API response.
type ResolvedCredential struct {
	CredentialID string // provider_credentials.id, or "" when Source == SourceLegacyProviderKey
	Name         string // for logs/audit — never the secret
	Secret       string
	HeaderName   string // e.g. "Authorization"
	Source       CredentialSource
}

// CredentialSource records which precedence tier produced the result — used
// only for observability (never exposed to end users).
type CredentialSource string

const (
	SourceProjectPinned     CredentialSource = "project_pinned"
	SourceProviderDefault   CredentialSource = "provider_default"
	SourceLegacyProviderKey CredentialSource = "legacy_provider_key"
)

// CredentialResolver implements the deterministic algorithm documented in
// migrations/062_provider_credentials.sql:
//
//  1. project_provider_access.credential_id for (project_id, provider_id), if
//     set — MUST be enabled or resolution fails closed (no fallback).
//  2. provider_credentials row with is_default=TRUE AND enabled=TRUE.
//  3. providers.api_key, ONLY if the provider has zero provider_credentials
//     rows at all (pure backward compatibility).
//  4. otherwise: ErrCredentialUnavailable.
type CredentialResolver struct {
	db      *sqlx.DB
	secrets *secretstore.Store
	creds   *ProviderCredentialStore
}

// NewCredentialResolver constructs a resolver. secrets may be nil ONLY in
// deployments that have zero provider_credentials rows and rely entirely on
// legacy providers.api_key — Resolve returns a clear configuration error if a
// decrypt is ever attempted without a configured secret store.
func NewCredentialResolver(db *sqlx.DB, secrets *secretstore.Store) *CredentialResolver {
	return &CredentialResolver{db: db, secrets: secrets, creds: NewProviderCredentialStore(db)}
}

// Resolve returns the credential that must handle a request to providerID on
// behalf of projectID. projectID may be "" (org-level/admin callers with no
// project context) — in that case step 1 is skipped and resolution proceeds
// straight to the provider default / legacy key.
func (r *CredentialResolver) Resolve(ctx context.Context, providerID, projectID string) (*ResolvedCredential, error) {
	// Step 1 — project-pinned credential.
	if projectID != "" {
		var pinned sql.NullString
		err := r.db.QueryRowContext(ctx, `
			SELECT credential_id::text
			FROM project_provider_access
			WHERE project_id::text = $1 AND provider_id::text = $2 AND enabled = TRUE`,
			projectID, providerID,
		).Scan(&pinned)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("credential resolver: loading project grant: %w", err)
		}
		if err == nil && pinned.Valid && pinned.String != "" {
			// Explicit pin — this MUST succeed on its own terms, no fallback.
			return r.loadByID(ctx, pinned.String, SourceProjectPinned)
		}
	}

	// Step 2 — provider default credential.
	var defaultID sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT id::text FROM provider_credentials
		WHERE provider_id::text = $1 AND is_default = TRUE AND enabled = TRUE
		LIMIT 1`, providerID,
	).Scan(&defaultID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("credential resolver: loading provider default: %w", err)
	}
	if err == nil && defaultID.Valid {
		return r.loadByID(ctx, defaultID.String, SourceProviderDefault)
	}

	// Step 3 — legacy providers.api_key, only if this provider has never been
	// migrated to the new credential pool at all.
	var credCount int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM provider_credentials WHERE provider_id::text = $1`, providerID,
	).Scan(&credCount); err != nil {
		return nil, fmt.Errorf("credential resolver: counting provider credentials: %w", err)
	}
	if credCount == 0 {
		var apiKey, apiKeyHeader string
		err := r.db.QueryRowContext(ctx, `
			SELECT api_key, COALESCE(api_key_header,'Authorization')
			FROM providers WHERE id::text = $1`, providerID,
		).Scan(&apiKey, &apiKeyHeader)
		if err != nil {
			return nil, fmt.Errorf("credential resolver: loading legacy provider key: %w", err)
		}
		if apiKey == "" {
			return nil, ErrCredentialUnavailable
		}
		return &ResolvedCredential{
			Secret: apiKey, HeaderName: apiKeyHeader, Source: SourceLegacyProviderKey,
		}, nil
	}

	// Step 4 — provider_credentials exist, but none is usable for this project.
	return nil, ErrCredentialUnavailable
}

func (r *CredentialResolver) loadByID(ctx context.Context, id string, source CredentialSource) (*ResolvedCredential, error) {
	var (
		name, ciphertext, providerHeader string
		apiKeyHeader                     sql.NullString
		enabled                          bool
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT c.name, c.secret_ciphertext, c.api_key_header, c.enabled,
		       COALESCE(p.api_key_header, 'Authorization')
		FROM provider_credentials c
		JOIN providers p ON p.id = c.provider_id
		WHERE c.id::text = $1`, id,
	).Scan(&name, &ciphertext, &apiKeyHeader, &enabled, &providerHeader)
	if errors.Is(err, sql.ErrNoRows) {
		// Pinned credential was deleted out from under the grant — fail closed.
		return nil, ErrCredentialUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("credential resolver: loading credential %s: %w", id, err)
	}
	if !enabled {
		// Pinned or default credential exists but is disabled — fail closed,
		// never silently pick a different credential.
		return nil, ErrCredentialUnavailable
	}
	if r.secrets == nil {
		return nil, fmt.Errorf("credential resolver: no secret store configured (set %s)", secretstore.KeyEnvVar)
	}
	secret, err := r.secrets.Decrypt(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("credential resolver: decrypting credential %s: %w", id, err)
	}
	header := providerHeader
	if apiKeyHeader.Valid && apiKeyHeader.String != "" {
		header = apiKeyHeader.String
	}
	r.creds.touchLastUsed(ctx, id)
	return &ResolvedCredential{
		CredentialID: id, Name: name, Secret: secret, HeaderName: header, Source: source,
	}, nil
}
