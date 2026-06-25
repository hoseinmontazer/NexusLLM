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
	"github.com/nexusllm/nexusllm/internal/alias"
	"github.com/nexusllm/nexusllm/internal/auth"
	"github.com/nexusllm/nexusllm/internal/config"
	"github.com/nexusllm/nexusllm/internal/gatewaypolicy"
	"github.com/nexusllm/nexusllm/internal/lifecycle"
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

	// ── Policy live reload every 60s ──────────────────────────────────────────
	// This ensures policy changes (RPM, TPD, max context) take effect without
	// restarting the gateway. The map is swapped atomically using a sync.Map
	// approach through the proxyHandler.
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-t.C:
				fresh := loadTeamPolicies(watchCtx, db, log)
				// Re-seed model permissions from DB in case they changed
				seedModelPermissions(watchCtx, db, policyEngine, log)
				// Replace the map entries in-place (same underlying map)
				for k, v := range fresh {
					teamPolicies[k] = v
				}
				// Remove teams that no longer exist
				for k := range teamPolicies {
					if _, ok := fresh[k]; !ok {
						delete(teamPolicies, k)
					}
				}
			}
		}
	}()

	// Lifecycle manager — unload endpoints idle for >30 min
	lifecycleMgr := lifecycle.NewManager(db, rdb, 30*time.Minute, nil, log)
	go lifecycleMgr.Start(watchCtx)

	// ── Runtime Manager (lazy-load architecture) ──────────────────────────────
	// Connects to the admin control plane's task manager via the DB.
	// The gateway uses it to start models on demand instead of returning 503.
	taskMgr := taskmanager.NewManager(db, log)

	// ── DB schema validation — fail fast ─────────────────────────────────────
	// Probes the live database constraint, not migration history.
	// Catches: migration never ran, ran against wrong DB, partial failure.
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
	idleMgr := runtimemgr.NewIdleManager(db, taskMgr, rmCfg, log).WithActivator(activator)
	go idleMgr.Start(watchCtx)

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

	// ── Proxy handler ─────────────────────────────────────────────────────────
	proxyHandler := proxy.NewHandler(
		policyEngine, gwPolicyEng, ppEngine, aliasRes,
		lifecycleMgr, registry, usageTracker, teamPolicies, log,
	).WithActivator(activator).WithDB(db)

	// ── Router ────────────────────────────────────────────────────────────────
	gin.SetMode(cfg.Server.Mode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(middleware.RequestID())
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
		v1.POST("/embeddings", proxyHandler.Embeddings)
		v1.GET("/models", proxyHandler.Models)

		// Multi-service APIs (AI Platform)
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
	type row struct {
		TeamID    string `db:"team_id"`
		ModelName string `db:"model_name"`
	}
	var rows []row
	_ = db.SelectContext(ctx, &rows, `
		SELECT tmp.team_id, m.name AS model_name
		FROM team_model_permissions tmp
		JOIN models m ON m.id = tmp.model_id
		WHERE m.enabled = TRUE`)
	for _, r := range rows {
		if err := engine.SetModelAllowed(ctx, r.TeamID, r.ModelName); err != nil {
			log.Warn("failed to seed model permission", zap.Error(err))
		}
	}
	log.Info("model permissions seeded", zap.Int("count", len(rows)))
}
