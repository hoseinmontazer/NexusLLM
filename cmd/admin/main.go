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
	_ "github.com/lib/pq"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/admin/handlers"
	"github.com/nexusllm/nexusllm/internal/alias"
	"github.com/nexusllm/nexusllm/internal/config"
	"github.com/nexusllm/nexusllm/internal/controller"
	"github.com/nexusllm/nexusllm/internal/gpu"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/nexusllm/nexusllm/internal/promptpolicy"
	"github.com/nexusllm/nexusllm/internal/runtime"
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

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatal("redis unreachable", zap.Error(err))
	}

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
	httpClient := &http.Client{Timeout: 10 * time.Second}
	factory := runtime.NewFactory(httpClient)
	registry, err := runtime.NewRegistry(db, rdb, factory, log)
	if err != nil {
		log.Warn("runtime registry init failed", zap.Error(err))
	}

	// ── Services ──────────────────────────────────────────────────────────────
	policyEngine    := policy.NewEngine(rdb)
	gpuInventory    := gpu.NewInventory(db, log)
	usageTracker    := usage.NewTracker(db, rdb, log)
	aliasResolver   := alias.NewResolver(db, rdb)
	ppEngine        := promptpolicy.NewEngine(db, rdb, log)
	dockerDriver    := controller.NewDockerDriver()
	modelCtrl       := controller.NewModelController(db, rdb, dockerDriver, log)

	// Start usage consumer in background
	usageCtx, usageCancel := context.WithCancel(ctx)
	defer usageCancel()
	go usageTracker.StartConsumer(usageCtx)

	// ── Handlers ──────────────────────────────────────────────────────────────
	orgH          := handlers.NewOrgHandler(db)
	teamH         := handlers.NewTeamHandler(db, rdb, policyEngine)
	apikeyH       := handlers.NewAPIKeyHandler(db, rdb)
	runtimeH      := handlers.NewRuntimeHandler(db, registry)
	controllerH   := handlers.NewControllerHandler(modelCtrl)
	gpuH          := handlers.NewGPUHandler(gpuInventory)
	usageH        := handlers.NewUsageHandler(usageTracker)
	aliasH        := handlers.NewAliasHandler(aliasResolver)
	ppH           := handlers.NewPromptPolicyHandler(ppEngine)

	// ── Router ────────────────────────────────────────────────────────────────
	gin.SetMode(cfg.Server.Mode)
	r := gin.New()
	r.Use(gin.Recovery(), gin.Logger())
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	admin := r.Group("/admin/v1")
	{
		// Organizations
		admin.POST("/orgs",       orgH.CreateOrg)
		admin.GET("/orgs",        orgH.ListOrgs)
		admin.GET("/orgs/:id",    orgH.GetOrg)
		admin.DELETE("/orgs/:id", orgH.DeactivateOrg)

		// Teams
		admin.POST("/orgs/:org_id/teams",        teamH.CreateTeam)
		admin.GET("/orgs/:org_id/teams",         teamH.ListTeams)
		admin.GET("/teams/:id",                  teamH.GetTeam)
		admin.DELETE("/teams/:id",               teamH.DeactivateTeam)
		admin.GET("/teams/:id/policy",           teamH.GetTeamPolicy)
		admin.PUT("/teams/:id/policy",           teamH.UpdateTeamPolicy)
		admin.POST("/teams/:id/models",          teamH.AddModelPermission)
		admin.DELETE("/teams/:id/models/:model", teamH.RemoveModelPermission)

		// API Keys
		admin.POST("/teams/:id/api-keys",  apikeyH.CreateAPIKey)
		admin.GET("/teams/:id/api-keys",   apikeyH.ListAPIKeys)
		admin.DELETE("/api-keys/:id",      apikeyH.RevokeAPIKey)

		// Runtime Model Registry
		admin.POST("/models",                        runtimeH.RegisterModel)
		admin.GET("/models",                         runtimeH.ListModels)
		admin.POST("/models/:id/endpoints",          runtimeH.AddEndpoint)
		admin.DELETE("/models/:id/endpoints/:ep_id", runtimeH.RemoveEndpoint)
		admin.POST("/models/:id/drain",              runtimeH.DrainModel)
		admin.POST("/models/:id/enable",             runtimeH.EnableModel)
		admin.POST("/models/:id/disable",            runtimeH.DisableModel)
		admin.PUT("/models/:id/runtime-config",      runtimeH.UpdateRuntimeConfig)
		admin.PUT("/models/:id/pool-strategy",       runtimeH.UpdatePoolStrategy)
		admin.GET("/models/:id/health",              runtimeH.GetModelHealth)

		// Model Controller (start/stop/restart/upgrade/rollback)
		admin.POST("/models/:id/start",    controllerH.StartModel)
		admin.POST("/models/:id/stop",     controllerH.StopModel)
		admin.POST("/models/:id/restart",  controllerH.RestartModel)
		admin.POST("/models/:id/upgrade",  controllerH.UpgradeModel)
		admin.POST("/models/:id/rollback", controllerH.RollbackModel)
		admin.GET("/models/:id/logs",      controllerH.GetModelLogs)

		// GPU Inventory
		admin.POST("/gpu/nodes",                          gpuH.RegisterNode)
		admin.GET("/gpu/nodes",                           gpuH.ListNodes)
		admin.POST("/gpu/nodes/:node_id/devices",         gpuH.RegisterDevice)
		admin.GET("/gpu/nodes/:node_id/devices",          gpuH.ListDevices)
		admin.POST("/gpu/pack",                           gpuH.PackModels)

		// Usage & Billing
		admin.GET("/usage/teams/:team_id",               usageH.GetTeamUsage)
		admin.GET("/usage/orgs/:org_id/monthly-spend",   usageH.GetOrgSpend)
		admin.POST("/usage/aggregate",                   usageH.TriggerAggregation)

		// Model Aliases (Virtual Models)
		admin.POST("/aliases",          aliasH.CreateAlias)
		admin.DELETE("/aliases",        aliasH.DeleteAlias)
		admin.GET("/aliases",           aliasH.ListAliases)
		admin.GET("/aliases/resolve",   aliasH.ResolveAlias)

		// Prompt Policies
		admin.POST("/prompt-policies",  ppH.CreatePolicy)
	}

	// ── Metrics ───────────────────────────────────────────────────────────────
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsSrv := &http.Server{Addr: ":" + cfg.Server.MetricsPort, Handler: metricsMux}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("metrics error", zap.Error(err))
		}
	}()

	adminPort := "8081"
	if p := os.Getenv("NEXUS_ADMIN_PORT"); p != "" {
		adminPort = p
	}
	srv := &http.Server{Addr: ":" + adminPort, Handler: r}
	go func() {
		log.Info("nexus-admin listening", zap.String("port", adminPort))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("admin server error", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("shutting down nexus-admin...")
	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	log.Info("nexus-admin stopped")
}
