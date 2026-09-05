package catalog

// credential_resolver_test.go — integration tests for the multi-provider
// credential routing feature (migration 062), run against a disposable
// postgres:16-alpine container with the full migration chain applied —
// mirrors the disposable-redis pattern already used in
// internal/policy/engine_test.go.
//
// Covers the exact acceptance scenario from the feature spec (section 40):
// two projects, two credentials for the same provider, isolation verified
// both ways, and the "grant removed -> fails closed, never falls back to the
// other project's credential" guarantee.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/nexusllm/nexusllm/internal/secretstore"
)

// setupTestPostgres starts a disposable postgres:16-alpine container and
// applies a minimal schema covering exactly the tables this feature reads and
// writes (organizations, teams, projects, providers, project_provider_access,
// provider_credentials) — final shape, matching migrations 001/011/031/047/
// 050/062. Skips the test entirely when Docker isn't available, matching
// internal/policy/engine_test.go's setupTestRedis convention.
//
// This deliberately does NOT replay the full migrations/ chain: doing so hit
// a pre-existing, unrelated bug (018_weighted_priority.sql's own
// scheduler_decisions.project_id index fails against a truly empty DB — see
// 018b_catchup_weighted.sql's and 018b_weighted_priority_fixup.sql's own
// comments, which independently confirm this exact failure mode. That bug is
// orthogonal to credential routing and out of scope for this change; it is
// called out separately in the deployment report. Using a minimal schema here
// keeps this test fast and decoupled from it while still exercising the real
// Postgres constraints (FKs, UNIQUE, the partial unique "one default per
// provider" index) that credential_resolver.go actually depends on.
func setupTestPostgres(t *testing.T) *sqlx.DB {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping credential resolver integration tests")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable — skipping credential resolver integration tests")
	}

	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	name := "nexus-test-pg-" + suffix
	port := 15400 + int(time.Now().UnixNano()%2000)

	run := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_USER=nexus", "-e", "POSTGRES_PASSWORD=nexus", "-e", "POSTGRES_DB=nexusllm",
		"-p", fmt.Sprintf("%d:5432", port),
		"postgres:16-alpine")
	if out, err := run.CombinedOutput(); err != nil {
		t.Skipf("could not start disposable postgres container (%v): %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	dsn := fmt.Sprintf("host=127.0.0.1 port=%d user=nexus password=nexus dbname=nexusllm sslmode=disable", port)
	var db *sqlx.DB
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		db, lastErr = sqlx.Open("postgres", dsn)
		if lastErr == nil {
			lastErr = db.Ping()
		}
		if lastErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("postgres never became ready: %v", lastErr)
	}

	if _, err := db.Exec(minimalTestSchema); err != nil {
		t.Fatalf("applying minimal test schema: %v", err)
	}
	return db
}

// minimalTestSchema mirrors the final shape (after all migrations) of just
// the tables this feature touches. Keep in sync with migrations/001_initial.sql,
// 011_projects.sql, 031_org_as_root.sql, 047_provider_catalog.sql,
// 050_provider_exposure_modes.sql, and 062_provider_credentials.sql.
const minimalTestSchema = `
CREATE TABLE organizations (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       VARCHAR(255) NOT NULL,
    slug       VARCHAR(100) NOT NULL UNIQUE,
    active     BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE teams (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name       VARCHAR(255) NOT NULL,
    slug       VARCHAR(100) NOT NULL,
    priority   INTEGER NOT NULL DEFAULT 5,
    active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(org_id, slug)
);

CREATE TABLE projects (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    team_id         UUID REFERENCES teams(id) ON DELETE CASCADE,
    name            VARCHAR(200) NOT NULL,
    status          VARCHAR(20) NOT NULL DEFAULT 'active',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(organization_id, name)
);

CREATE TABLE providers (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name              TEXT NOT NULL UNIQUE,
    display_name      TEXT NOT NULL,
    backend_type      TEXT NOT NULL,
    base_url          TEXT NOT NULL,
    api_key           TEXT NOT NULL DEFAULT '',
    api_key_header    TEXT NOT NULL DEFAULT 'Authorization',
    exposure_mode     TEXT NOT NULL DEFAULT 'managed',
    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE project_provider_access (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id       UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    provider_id      UUID NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    allowed_prefixes TEXT[] NOT NULL DEFAULT '{}',
    denied_prefixes  TEXT[] NOT NULL DEFAULT '{}',
    enabled          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, provider_id)
);

CREATE TABLE provider_credentials (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id       UUID NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    secret_ciphertext TEXT NOT NULL,
    api_key_header    TEXT,
    is_default        BOOLEAN NOT NULL DEFAULT FALSE,
    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    metadata          JSONB NOT NULL DEFAULT '{}',
    last_used_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider_id, name)
);

CREATE UNIQUE INDEX idx_provider_credentials_one_default
    ON provider_credentials(provider_id) WHERE is_default = TRUE;

ALTER TABLE project_provider_access
    ADD COLUMN credential_id UUID REFERENCES provider_credentials(id) ON DELETE SET NULL;
`

func testSecretStore(t *testing.T) *secretstore.Store {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	s, err := secretstore.New(key)
	if err != nil {
		t.Fatalf("secretstore.New: %v", err)
	}
	return s
}

// seedOrgProjectProvider creates one organization, one team, two projects, and
// one provider in catalog/hybrid exposure mode. Returns their IDs.
func seedOrgProjectProvider(t *testing.T, db *sqlx.DB) (orgID, teamID, projectA, projectB, providerID string) {
	t.Helper()
	ctx := context.Background()
	orgID = uuid.New().String()
	teamID = uuid.New().String()
	projectA = uuid.New().String()
	projectB = uuid.New().String()
	providerID = uuid.New().String()

	mustExec(t, db, `INSERT INTO organizations (id, name, slug) VALUES ($1,$2,$3)`,
		orgID, "acceptance-test-org", "acceptance-test-org-"+orgID[:8])
	mustExec(t, db, `INSERT INTO teams (id, org_id, name, slug) VALUES ($1,$2,$3,$4)`,
		teamID, orgID, "team", "team-"+teamID[:8])
	mustExec(t, db, `INSERT INTO projects (id, organization_id, team_id, name) VALUES ($1,$2,$3,$4)`,
		projectA, orgID, teamID, "project-a")
	mustExec(t, db, `INSERT INTO projects (id, organization_id, team_id, name) VALUES ($1,$2,$3,$4)`,
		projectB, orgID, teamID, "project-b")
	mustExec(t, db, `
		INSERT INTO providers (id, name, display_name, backend_type, base_url, exposure_mode)
		VALUES ($1, $2, 'OpenRouter', 'openrouter_provider', 'https://openrouter.ai', 'catalog')`,
		providerID, "openrouter-"+providerID[:8])
	_ = ctx
	return
}

func mustExec(t *testing.T, db *sqlx.DB, query string, args ...interface{}) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("seed exec failed (%s): %v", query, err)
	}
}

// TestAcceptance_TwoProjectsTwoCredentials is the end-to-end scenario from
// the feature spec's "Acceptance Test" section: two projects, two OpenRouter
// credentials, isolation verified both directions, then the grant removed for
// project B and resolution must fail closed — NEVER fall back to project A's
// credential.
func TestAcceptance_TwoProjectsTwoCredentials(t *testing.T) {
	db := setupTestPostgres(t)
	defer db.Close()
	secrets := testSecretStore(t)
	_, _, projectA, projectB, providerID := seedOrgProjectProvider(t, db)

	credStore := NewProviderCredentialStore(db)
	ctx := context.Background()

	cipherA, err := secrets.Encrypt("sk-or-v1-AAAA-project-a-token")
	if err != nil {
		t.Fatalf("encrypt A: %v", err)
	}
	credA, err := credStore.Create(ctx, CreateInput{ProviderID: providerID, Name: "credential-a"}, cipherA)
	if err != nil {
		t.Fatalf("create credential A: %v", err)
	}

	cipherB, err := secrets.Encrypt("sk-or-v1-BBBB-project-b-token")
	if err != nil {
		t.Fatalf("encrypt B: %v", err)
	}
	credB, err := credStore.Create(ctx, CreateInput{ProviderID: providerID, Name: "credential-b"}, cipherB)
	if err != nil {
		t.Fatalf("create credential B: %v", err)
	}

	// Assignments: project-A -> credential-A, project-B -> credential-B.
	grantIDB := uuid.New().String()
	mustExec(t, db, `
		INSERT INTO project_provider_access (id, project_id, provider_id, credential_id)
		VALUES ($1,$2,$3,$4)`, uuid.New().String(), projectA, providerID, credA.ID)
	mustExec(t, db, `
		INSERT INTO project_provider_access (id, project_id, provider_id, credential_id)
		VALUES ($1,$2,$3,$4)`, grantIDB, projectB, providerID, credB.ID)

	resolver := NewCredentialResolver(db, secrets)

	resA, err := resolver.Resolve(ctx, providerID, projectA)
	if err != nil {
		t.Fatalf("resolve project A: %v", err)
	}
	if resA.Secret != "sk-or-v1-AAAA-project-a-token" {
		t.Fatalf("project A got wrong secret: %q", resA.Secret)
	}
	if resA.CredentialID != credA.ID {
		t.Fatalf("project A got wrong credential ID: got %s want %s", resA.CredentialID, credA.ID)
	}

	resB, err := resolver.Resolve(ctx, providerID, projectB)
	if err != nil {
		t.Fatalf("resolve project B: %v", err)
	}
	if resB.Secret != "sk-or-v1-BBBB-project-b-token" {
		t.Fatalf("project B got wrong secret: %q", resB.Secret)
	}
	if resB.CredentialID != credB.ID {
		t.Fatalf("project B got wrong credential ID: got %s want %s", resB.CredentialID, credB.ID)
	}
	if resA.Secret == resB.Secret {
		t.Fatalf("CREDENTIAL ISOLATION VIOLATED: project A and B resolved to the same secret")
	}

	// Remove project B's grant entirely.
	mustExec(t, db, `DELETE FROM project_provider_access WHERE id = $1`, grantIDB)

	// Project B must now fail closed — no provider default exists, no legacy
	// key, so ErrCredentialUnavailable. Critically, it must NOT resolve to
	// credential A just because A is enabled and reachable via the same provider.
	_, err = resolver.Resolve(ctx, providerID, projectB)
	if err != ErrCredentialUnavailable {
		t.Fatalf("expected ErrCredentialUnavailable for project B after grant removal, got: %v", err)
	}

	// Project A must be completely unaffected by project B's grant removal.
	resA2, err := resolver.Resolve(ctx, providerID, projectA)
	if err != nil {
		t.Fatalf("resolve project A after B's grant removed: %v", err)
	}
	if resA2.CredentialID != credA.ID {
		t.Fatalf("project A's resolution changed after an unrelated project's grant was removed")
	}
}

// TestResolve_PinnedCredentialDisabled_FailsClosed proves section 4/9's core
// isolation invariant: a project explicitly pinned to a credential must NEVER
// silently fall back to a different credential (e.g. the provider default)
// just because its pinned credential became disabled.
func TestResolve_PinnedCredentialDisabled_FailsClosed(t *testing.T) {
	db := setupTestPostgres(t)
	defer db.Close()
	secrets := testSecretStore(t)
	_, _, projectA, _, providerID := seedOrgProjectProvider(t, db)
	ctx := context.Background()

	credStore := NewProviderCredentialStore(db)
	cipherPinned, _ := secrets.Encrypt("sk-pinned")
	pinned, err := credStore.Create(ctx, CreateInput{ProviderID: providerID, Name: "pinned"}, cipherPinned)
	if err != nil {
		t.Fatalf("create pinned credential: %v", err)
	}
	cipherDefault, _ := secrets.Encrypt("sk-default")
	if _, err := credStore.Create(ctx, CreateInput{ProviderID: providerID, Name: "default", IsDefault: true}, cipherDefault); err != nil {
		t.Fatalf("create default credential: %v", err)
	}

	mustExec(t, db, `
		INSERT INTO project_provider_access (id, project_id, provider_id, credential_id)
		VALUES ($1,$2,$3,$4)`, uuid.New().String(), projectA, providerID, pinned.ID)

	resolver := NewCredentialResolver(db, secrets)

	// Sanity: resolves fine while enabled.
	if res, err := resolver.Resolve(ctx, providerID, projectA); err != nil || res.Secret != "sk-pinned" {
		t.Fatalf("expected pinned credential to resolve while enabled: res=%+v err=%v", res, err)
	}

	// Disable the pinned credential.
	if err := credStore.SetEnabled(ctx, pinned.ID, false); err != nil {
		t.Fatalf("disable pinned credential: %v", err)
	}

	_, err = resolver.Resolve(ctx, providerID, projectA)
	if err != ErrCredentialUnavailable {
		t.Fatalf("expected ErrCredentialUnavailable when the pinned credential is disabled, got: %v "+
			"(a non-nil different result would mean it silently fell back to the provider default — "+
			"a billing isolation violation)", err)
	}
}

// TestResolve_NoGrant_FallsBackToProviderDefault proves the documented
// "absence of an explicit pin = use the provider default" behaviour (distinct
// from "explicit pin to a now-disabled credential = fail closed").
func TestResolve_NoGrant_FallsBackToProviderDefault(t *testing.T) {
	db := setupTestPostgres(t)
	defer db.Close()
	secrets := testSecretStore(t)
	_, _, projectA, _, providerID := seedOrgProjectProvider(t, db)
	ctx := context.Background()

	credStore := NewProviderCredentialStore(db)
	cipherDefault, _ := secrets.Encrypt("sk-default-only")
	def, err := credStore.Create(ctx, CreateInput{ProviderID: providerID, Name: "only-default", IsDefault: true}, cipherDefault)
	if err != nil {
		t.Fatalf("create default credential: %v", err)
	}

	// No project_provider_access row at all for projectA.
	resolver := NewCredentialResolver(db, secrets)
	res, err := resolver.Resolve(ctx, providerID, projectA)
	if err != nil {
		t.Fatalf("resolve with no grant: %v", err)
	}
	if res.CredentialID != def.ID || res.Secret != "sk-default-only" {
		t.Fatalf("expected fallback to provider default, got %+v", res)
	}
	if res.Source != SourceProviderDefault {
		t.Fatalf("expected SourceProviderDefault, got %s", res.Source)
	}
}

// TestResolve_LegacyProviderKey_BackwardCompatible proves that a provider
// which has never been migrated to the credential pool (zero
// provider_credentials rows) keeps working exactly as before — falling back
// to the legacy providers.api_key column.
func TestResolve_LegacyProviderKey_BackwardCompatible(t *testing.T) {
	db := setupTestPostgres(t)
	defer db.Close()
	secrets := testSecretStore(t)
	_, _, projectA, _, providerID := seedOrgProjectProvider(t, db)
	ctx := context.Background()

	mustExec(t, db, `UPDATE providers SET api_key = 'legacy-plaintext-key' WHERE id::text = $1`, providerID)

	// Zero provider_credentials rows for this provider, zero grants.
	resolver := NewCredentialResolver(db, secrets)
	res, err := resolver.Resolve(ctx, providerID, projectA)
	if err != nil {
		t.Fatalf("resolve legacy provider: %v", err)
	}
	if res.Secret != "legacy-plaintext-key" {
		t.Fatalf("expected legacy providers.api_key fallback, got %+v", res)
	}
	if res.Source != SourceLegacyProviderKey {
		t.Fatalf("expected SourceLegacyProviderKey, got %s", res.Source)
	}

	// Also verify the no-project-context path (admin/sync callers) resolves
	// identically — projectID == "".
	res2, err := resolver.Resolve(ctx, providerID, "")
	if err != nil {
		t.Fatalf("resolve legacy provider with no project context: %v", err)
	}
	if res2.Secret != "legacy-plaintext-key" {
		t.Fatalf("expected legacy providers.api_key fallback with empty project, got %+v", res2)
	}
}

// TestResolve_DeterministicOrdering proves resolution never depends on
// Postgres's arbitrary row order: creating credentials in one order and
// querying in another must not change which one is selected as default.
func TestResolve_DeterministicOrdering(t *testing.T) {
	db := setupTestPostgres(t)
	defer db.Close()
	secrets := testSecretStore(t)
	_, _, projectA, _, providerID := seedOrgProjectProvider(t, db)
	ctx := context.Background()
	credStore := NewProviderCredentialStore(db)

	// Create several non-default credentials, then set the LAST one as
	// default — the partial unique index guarantees exactly one default
	// regardless of creation order.
	var lastID string
	for i := 0; i < 5; i++ {
		cipher, _ := secrets.Encrypt(fmt.Sprintf("sk-%d", i))
		c, err := credStore.Create(ctx, CreateInput{ProviderID: providerID, Name: fmt.Sprintf("cred-%d", i)}, cipher)
		if err != nil {
			t.Fatalf("create credential %d: %v", i, err)
		}
		lastID = c.ID
	}
	mustExec(t, db, `UPDATE provider_credentials SET is_default = TRUE WHERE id::text = $1`, lastID)

	resolver := NewCredentialResolver(db, secrets)
	for i := 0; i < 10; i++ {
		res, err := resolver.Resolve(ctx, providerID, projectA)
		if err != nil {
			t.Fatalf("resolve iteration %d: %v", i, err)
		}
		if res.CredentialID != lastID {
			t.Fatalf("iteration %d: resolution is non-deterministic — got %s, want %s", i, res.CredentialID, lastID)
		}
	}
}

// TestProviderCredentialStore_OnlyOneDefault proves the "at most one default
// per provider" invariant holds even when Create is called with IsDefault
// twice for the same provider.
func TestProviderCredentialStore_OnlyOneDefault(t *testing.T) {
	db := setupTestPostgres(t)
	defer db.Close()
	secrets := testSecretStore(t)
	_, _, _, _, providerID := seedOrgProjectProvider(t, db)
	ctx := context.Background()
	credStore := NewProviderCredentialStore(db)

	cipher1, _ := secrets.Encrypt("sk-1")
	first, err := credStore.Create(ctx, CreateInput{ProviderID: providerID, Name: "first", IsDefault: true}, cipher1)
	if err != nil {
		t.Fatalf("create first default: %v", err)
	}
	cipher2, _ := secrets.Encrypt("sk-2")
	second, err := credStore.Create(ctx, CreateInput{ProviderID: providerID, Name: "second", IsDefault: true}, cipher2)
	if err != nil {
		t.Fatalf("create second default: %v", err)
	}

	var defaultCount int
	if err := db.Get(&defaultCount, `
		SELECT COUNT(*) FROM provider_credentials WHERE provider_id::text = $1 AND is_default = TRUE`,
		providerID); err != nil {
		t.Fatalf("counting defaults: %v", err)
	}
	if defaultCount != 1 {
		t.Fatalf("expected exactly 1 default credential, got %d", defaultCount)
	}

	got, err := credStore.Get(ctx, second.ID)
	if err != nil || !got.IsDefault {
		t.Fatalf("expected second credential to be the current default: %+v err=%v", got, err)
	}
	firstNow, err := credStore.Get(ctx, first.ID)
	if err != nil || firstNow.IsDefault {
		t.Fatalf("expected first credential to no longer be default: %+v err=%v", firstNow, err)
	}
}
