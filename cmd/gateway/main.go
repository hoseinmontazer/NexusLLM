package main

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/nexusllm/nexusllm/internal/admission"
	"github.com/nexusllm/nexusllm/internal/alias"
	"github.com/nexusllm/nexusllm/internal/auth"
	"github.com/nexusllm/nexusllm/internal/billing"
	billingsweep "github.com/nexusllm/nexusllm/internal/billing/sweep"
	"github.com/nexusllm/nexusllm/internal/catalog"
	"github.com/nexusllm/nexusllm/internal/config"
	"github.com/nexusllm/nexusllm/internal/gatewaypolicy"
	"github.com/nexusllm/nexusllm/internal/middleware"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/nexusllm/nexusllm/internal/promptpolicy"
	"github.com/nexusllm/nexusllm/internal/proxy"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"github.com/nexusllm/nexusllm/internal/runtimemgr"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
	"github.com/nexusllm/nexusllm/internal/usage"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync()

	cfg, err := config.Load()
	if err != nil {
		log.Fatal("failed to load config", zap.Error(err))
	}

	// ── Redis ─────────────────────────────────────────────────────────────────
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatal("redis unreachable", zap.Error(err))
	}

	// ── PostgreSQL ────────────────────────────────────────────────────────────
	sqlDB, err := sql.Open("postgres", cfg.Database.DSN)
	if err != nil {
		log.Fatal("failed to open postgres", zap.Error(err))
	}
	sqlDB.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.Database.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.Database.ConnMaxLifetime)
	if err := sqlDB.PingContext(ctx); err != nil {
		log.Fatal("postgres unreachable", zap.Error(err))
	}
	db := sqlx.NewDb(sqlDB, "postgres")
	log.Info("postgres + redis connected")

	// ── Runtime registry ──────────────────────────────────────────────────────
	httpClient := &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	factory := runtime.NewFactory(httpClient)
	if cfg.Upstream.Proxy != "" {
		factory.SetGlobalProxy(cfg.Upstream.Proxy)
		log.Info("global upstream proxy configured", zap.String("proxy", cfg.Upstream.Proxy))
	}
	registry, err := runtime.NewRegistry(db, rdb, factory, log)
	if err != nil {
		log.Warn("runtime registry init failed — starting with empty registry (run migrations)",
			zap.Error(err))
		// Build an empty registry so the gateway can still start.
		// Models will become available once migrations run and the registry reloads.
		registry, _ = runtime.NewEmptyRegistry(db, rdb, factory, log)
	}

	// ── Runtime watcher ───────────────────────────────────────────────────────
	watcher := runtime.NewWatcher(registry, db, log, cfg.VLLM.PollInterval)
	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	go watcher.Start(watchCtx)

	// ── Registry auto-reload every 10s ───────────────────────────────────────
	// Short interval ensures HA replicas and recovered runtimes are routable
	// within 10 seconds of becoming healthy. The 60s reload was too slow for
	// failover — a dead node can go offline in 5 min but we want < 30s
	// routing recovery once the replacement runtime is ready.
	go registry.StartPeriodicReload(watchCtx, 10*time.Second)

	// ── Services ──────────────────────────────────────────────────────────────
	authSvc := auth.NewService(rdb, db, cfg.Auth.JWTSecret, cfg.Auth.APIKeyCacheTTL)
	policyEngine := policy.NewEngine(rdb)
	gwPolicyEng := gatewaypolicy.NewEngine(db, rdb, log)
	ppEngine := promptpolicy.NewEngine(db, rdb, log)
	aliasRes := alias.NewResolver(db, rdb)
	usageTracker := usage.NewTracker(db, rdb, log)
	teamPolicies := loadTeamPolicies(ctx, db, log)

	// Lifecycle manager removed — activity tracking is now owned exclusively by
	// RuntimeActivator (runtimemgr.Activator.RecordActivity), which writes
	// last_used_at directly to agent_runtimes. The old Redis-based lifecycle
	// manager is no longer instantiated. (Phase 1 of architecture cleanup.)

	// ── Runtime Manager (lazy-load architecture) ──────────────────────────────
	taskMgr := taskmanager.NewManager(db, log)

	if err := taskMgr.ValidateSchema(ctx); err != nil {
		log.Fatal("database schema incompatible — run pending migrations before starting",
			zap.Error(err),
		)
	}
	rmCfg := runtimemgr.DefaultConfig()
	rmCfg.DefaultIdleTimeout = cfg.RuntimeMgr.DefaultIdleTimeout
	rmCfg.ColdStartTimeout = cfg.RuntimeMgr.ColdStartTimeout
	rmCfg.DefaultModelsVolume = cfg.RuntimeMgr.DefaultModelsVolume
	rmCfg.DefaultImage = cfg.RuntimeMgr.DefaultImage
	guard := runtimemgr.NewResourceGuard(db, log)
	activator := runtimemgr.NewActivator(db, taskMgr, registry, guard, rmCfg, log)
	startTracker := runtimemgr.NewStartTracker()
	idleMgr := runtimemgr.NewIdleManager(db, taskMgr, rmCfg, log).WithActivator(activator)
	go idleMgr.Start(watchCtx)

	// IMPROVEMENT-5: background sweep that detects and auto-corrects any
	// divergence between agent_runtimes.bind_port and model_endpoints.port.
	// Runs every 5 minutes. Emits a warning log for every corrected row.
	activator.StartPortMismatchSweep(watchCtx)

	// Usage consumer
	go usageTracker.StartConsumer(watchCtx)

	// Usage aggregation every hour
	go func() {
		t := time.NewTicker(1 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-t.C:
				usageTracker.Aggregate(context.Background())
			}
		}
	}()

	seedModelPermissions(ctx, db, policyEngine, log)
	seedProjectPolicies(ctx, db, policyEngine, log)
	seedProjectProviderAccess(ctx, db, policyEngine, log)

	// ── Admission gate ────────────────────────────────────────────────────────
	// Uses a dedicated standalone Redis instance (redis-admission) to avoid
	// Redis Cluster cross-slot violations. Falls back to PostgreSQL checks
	// when admission Redis is disabled or unavailable.
	var admissionGate *admission.Gate
	if cfg.AdmissionRedis.Enabled {
		admissionRDB, admErr := admission.NewClient(admission.Config{
			Addr:     cfg.AdmissionRedis.Addr,
			Password: cfg.AdmissionRedis.Password,
			DB:       cfg.AdmissionRedis.DB,
		})
		if admErr != nil {
			log.Warn("admission redis unavailable — admission gate will use PostgreSQL fallback",
				zap.Error(admErr))
		} else {
			admissionGate = admission.NewGate(admissionRDB, db, log)
			log.Info("admission gate initialised",
				zap.String("addr", cfg.AdmissionRedis.Addr))

			// Rebuild admission Redis counters from PostgreSQL on startup.
			rebuilder := billingsweep.NewRedisRebuilder(db, admissionRDB, log)
			if err := rebuilder.Run(ctx); err != nil {
				log.Warn("admission redis rebuild error on startup", zap.Error(err))
			}
			// Periodic rebuild every 6 hours as a safety net.
			go rebuilder.Start(watchCtx, 6*time.Hour)
		}
	} else {
		log.Info("admission redis disabled — gate operates in PostgreSQL-fallback mode")
	}

	// ── Billing services ──────────────────────────────────────────────────────
	expiryPolicy := billing.ExpiryRecoveryPolicy(cfg.Billing.ExpiryRecoveryPolicy)
	billingReserver := billing.NewReserver(db, log).WithAuthTTL(cfg.Billing.AuthTTL)
	billingSettler  := billing.NewSettler(db, log, expiryPolicy)

	// ── Billing background sweeps ─────────────────────────────────────────────
	// All sweeps are idempotent and safe to run concurrently with requests.
	pendingSweep := billingsweep.NewPendingSweep(db, billingSettler, billingReserver, log)
	expirySweep  := billingsweep.NewExpirySweep(db, log)
	walletRecon  := billingsweep.NewWalletReconciler(db, log)
	quotaSync    := billingsweep.NewQuotaLedgerSync(db, log)

	go pendingSweep.Start(watchCtx, 5*time.Minute)
	go expirySweep.Start(watchCtx, 2*time.Minute)
	go walletRecon.Start(watchCtx, 1*time.Hour)
	go quotaSync.Start(watchCtx, 15*time.Minute)

	log.Info("billing sweeps started")

	// Keep billing and admission variables alive (used by proxy handler below).
	_ = admissionGate
	_ = billingReserver
	_ = billingSettler

	// ── Proxy handler ─────────────────────────────────────────────────────────
	catalogResolver := catalog.NewVirtualModelResolver(db, log)
	capValidator := proxy.NewCapabilityValidator(registry).WithCatalogResolver(catalogResolver)
	proxyHandler := proxy.NewHandler(
		policyEngine, gwPolicyEng, ppEngine, aliasRes,
		registry, usageTracker, teamPolicies, log,
	).WithActivator(activator).WithDB(db).WithColdStartTimeout(rmCfg.ColdStartTimeout).
		WithStartTracker(startTracker).
		WithCapabilityValidator(capValidator).WithFactory(factory).
		WithVirtualResolver(catalogResolver)

	// ── Policy live reload every 60s ──────────────────────────────────────────
	// Uses a sync.RWMutex-protected wrapper to avoid data races between the
	// reload goroutine (writer) and request handlers (readers).
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-t.C:
				fresh := loadTeamPolicies(watchCtx, db, log)
				seedModelPermissions(watchCtx, db, policyEngine, log)
				seedProjectPolicies(watchCtx, db, policyEngine, log)
				seedProjectProviderAccess(watchCtx, db, policyEngine, log)
				proxyHandler.SwapTeamPolicies(fresh)
			}
		}
	}()

	// ── Router ────────────────────────────────────────────────────────────────
	gin.SetMode(cfg.Server.Mode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(middleware.RequestID())
	r.Use(middleware.AccessLog(log))
	r.Use(middleware.MetricsMiddleware())

	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/readyz", func(c *gin.Context) {
		if err := rdb.Ping(c.Request.Context()).Err(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not ready"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready", "models": registry.ListModels()})
	})

	v1 := r.Group("/v1", middleware.AuthRequired(authSvc))
	{
		v1.POST("/chat/completions", proxyHandler.ChatCompletions)
		v1.POST("/completions", proxyHandler.LegacyCompletions) // legacy text completions (Roo Code, Continue FIM)
		v1.POST("/embeddings", proxyHandler.Embeddings)
		v1.GET("/models", proxyHandler.Models)
		v1.GET("/models/:model_id", proxyHandler.ModelByID) // single-model lookup (Cline, Continue, Kilo Code)

		// Provider passthrough — returns the raw /models response from a
		// configured provider without any transformation. Useful for browsing
		// the full provider catalog with all provider-specific metadata intact.
		// GET /v1/providers/:provider_name/models
		// e.g. GET /v1/providers/openrouter/models
		v1.GET("/providers/:provider_name/models", proxyHandler.ProviderModels)

		// Additional inference endpoints
		v1.POST("/rerank", proxyHandler.Rerank)
		v1.POST("/audio/transcriptions", proxyHandler.Transcriptions)
		v1.POST("/audio/speech", proxyHandler.Speech)
		v1.POST("/ocr", proxyHandler.OCR)
	}

	// ── Metrics server ────────────────────────────────────────────────────────
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsSrv := &http.Server{Addr: ":" + cfg.Server.MetricsPort, Handler: metricsMux}
	go func() {
		log.Info("metrics listening", zap.String("port", cfg.Server.MetricsPort))
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("metrics server error", zap.Error(err))
		}
	}()

	// ── Main server ───────────────────────────────────────────────────────────
	srv := &http.Server{
		Addr:         ":" + cfg.Server.Port,
		Handler:      r,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}
	go func() {
		log.Info("nexus-gateway listening", zap.String("port", cfg.Server.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("gateway error", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("shutting down nexus-gateway...")
	watchCancel()
	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	_ = metricsSrv.Shutdown(shutCtx)
	log.Info("nexus-gateway stopped")
}

func loadTeamPolicies(ctx context.Context, db *sqlx.DB, log *zap.Logger) map[string]*policy.TeamPolicy {
	type row struct {
		TeamID           string `db:"team_id"`
		RPM              int    `db:"rpm"`
		TPD              int    `db:"tpd"`
		MaxConcurrent    int    `db:"max_concurrent"`
		MaxContextTokens int    `db:"max_context_tokens"`
	}
	var rows []row
	if err := db.SelectContext(ctx, &rows,
		`SELECT team_id, rpm, tpd, max_concurrent, max_context_tokens FROM policies`); err != nil {
		log.Warn("could not load team policies", zap.Error(err))
		return map[string]*policy.TeamPolicy{}
	}
	m := make(map[string]*policy.TeamPolicy, len(rows))
	for _, r := range rows {
		m[r.TeamID] = &policy.TeamPolicy{
			RPMLimit: r.RPM, TPDLimit: r.TPD,
			MaxConcurrent: r.MaxConcurrent, MaxContextTokens: r.MaxContextTokens,
		}
	}
	log.Info("team policies loaded", zap.Int("count", len(m)))
	return m
}

func seedModelPermissions(ctx context.Context, db *sqlx.DB, engine *policy.Engine, log *zap.Logger) {
	// Seed org-level ACL sets (canonical — used by the policy engine Step 0)
	// and the legacy team-level sets (fallback for pre-031 team-only keys).
	//
	// Unified architecture: every Public Model — local runtime or remote
	// provider — has a row in the models table and a grant in
	// team_model_permissions. One query covers every backend type.
	// There is no second seeding path for virtual or remote models.
	type row struct {
		OrgID     string `db:"org_id"`
		TeamID    string `db:"team_id"`
		ModelName string `db:"model_name"`
	}
	var rows []row
	_ = db.SelectContext(ctx, &rows, `
		SELECT t.org_id, tmp.team_id, m.name AS model_name
		FROM team_model_permissions tmp
		JOIN models m ON m.id = tmp.model_id
		JOIN teams  t ON t.id  = tmp.team_id
		WHERE m.enabled = TRUE`)
	for _, r := range rows {
		if err := engine.SetOrgModelAllowed(ctx, r.OrgID, r.ModelName); err != nil {
			log.Warn("failed to seed org model permission", zap.Error(err))
		}
		if err := engine.SetModelAllowed(ctx, r.TeamID, r.ModelName); err != nil {
			log.Warn("failed to seed team model permission", zap.Error(err))
		}
	}
	log.Info("model permissions seeded", zap.Int("count", len(rows)))
}

// seedProjectPolicies loads all project_policies rows and pushes them into the
// Redis Layer-1 policy cache. Called at startup and on the 60s reload cycle so
// the gateway hot path never needs a DB round-trip for policy evaluation.
func seedProjectPolicies(ctx context.Context, db *sqlx.DB, engine *policy.Engine, log *zap.Logger) {
	type row struct {
		ProjectID          string `db:"project_id"`
		RPM                int    `db:"rpm"`
		TPM                int    `db:"tpm"`
		MaxConcurrent      int    `db:"max_concurrent"`
		MaxContextTokens   int    `db:"max_context_tokens"`
		DailyTokenBudget   int64  `db:"daily_token_budget"`
		MonthlyTokenBudget int64  `db:"monthly_token_budget"`
	}
	var rows []row
	if err := db.SelectContext(ctx, &rows,
		`SELECT project_id::text, rpm, tpm, max_concurrent, max_context_tokens,
		        daily_token_budget, monthly_token_budget
		 FROM project_policies`); err != nil {
		log.Warn("could not seed project policies (table may not exist yet)", zap.Error(err))
		return
	}
	for _, r := range rows {
		_ = engine.SetProjectPolicy(ctx, r.ProjectID, policy.ProjectPolicy{
			RPMLimit:           r.RPM,
			TPMLimit:           r.TPM,
			MaxConcurrent:      r.MaxConcurrent,
			MaxContextTokens:   r.MaxContextTokens,
			DailyTokenBudget:   r.DailyTokenBudget,
			MonthlyTokenBudget: r.MonthlyTokenBudget,
		})
	}
	log.Info("project policies seeded", zap.Int("count", len(rows)))
}

// seedProjectProviderAccess loads all project_provider_access rows for
// catalog/hybrid providers and pushes them into the Redis vproviders hash
// for each project. Called at startup and on the 60s reload cycle.
//
// This is the bridge between the DB-authoritative grant table and the
// Redis-backed hot-path ACL check in policy.Engine.Evaluate().
func seedProjectProviderAccess(ctx context.Context, db *sqlx.DB, engine *policy.Engine, log *zap.Logger) {
	store := catalog.NewProjectProviderAccessStore(db)
	grants, err := store.ListAll(ctx)
	if err != nil {
		log.Warn("could not seed project provider access (table may not exist yet)", zap.Error(err))
		return
	}
	// We also need the expose_prefix for each provider so the Redis entry
	// carries the correct prefix string for the fast-path prefix check.
	// Load a name→prefix map from providers.
	type provRow struct {
		ID                  string `db:"id"`
		Name                string `db:"name"`
		CatalogExposePrefix string `db:"catalog_expose_prefix"`
	}
	var provRows []provRow
	_ = db.SelectContext(ctx, &provRows,
		`SELECT id::text, name, catalog_expose_prefix FROM providers
		 WHERE enabled=TRUE AND exposure_mode IN ('catalog','hybrid')`)
	prefixByID := make(map[string]string, len(provRows))
	for _, r := range provRows {
		prefix := r.CatalogExposePrefix
		if prefix == "" {
			prefix = r.Name
		}
		prefixByID[r.ID] = prefix
	}

	for _, g := range grants {
		prefix := prefixByID[g.ProviderID]
		if prefix == "" {
			prefix = g.ProviderName
		}
		entry := policy.ProviderAccessEntry{
			ProviderPrefix:  prefix,
			AllowedPatterns: g.AllowedPrefixes,
			DeniedPatterns:  g.DeniedPrefixes,
		}
		if err := engine.SetProjectProviderAccess(ctx, g.ProjectID, g.ProviderID, entry); err != nil {
			log.Warn("failed to seed project provider access",
				zap.String("project_id", g.ProjectID),
				zap.String("provider_id", g.ProviderID),
				zap.Error(err),
			)
		}
	}
	log.Info("project provider access seeded", zap.Int("count", len(grants)))
}
