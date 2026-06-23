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
	"github.com/nexusllm/nexusllm/internal/agentauth"
	"github.com/nexusllm/nexusllm/internal/alias"
	"github.com/nexusllm/nexusllm/internal/config"
	"github.com/nexusllm/nexusllm/internal/controller"
	"github.com/nexusllm/nexusllm/internal/gpu"
	"github.com/nexusllm/nexusllm/internal/nodeagent"
	"github.com/nexusllm/nexusllm/internal/nodehealth"
	"github.com/nexusllm/nexusllm/internal/placement"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/nexusllm/nexusllm/internal/promptpolicy"
	"github.com/nexusllm/nexusllm/internal/project"
	"github.com/nexusllm/nexusllm/internal/preemption"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"github.com/nexusllm/nexusllm/internal/services"
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
	httpClient := &http.Client{Timeout: 10 * time.Second}
	factory := runtime.NewFactory(httpClient)
	registry, err := runtime.NewRegistry(db, rdb, factory, log)
	if err != nil {
		log.Warn("runtime registry init failed", zap.Error(err))
		registry, _ = runtime.NewEmptyRegistry(db, rdb, factory, log)
	}

	// ── Services ──────────────────────────────────────────────────────────────
	policyEngine   := policy.NewEngine(rdb)
	gpuInventory   := gpu.NewInventory(db, log)
	usageTracker   := usage.NewTracker(db, rdb, log)
	aliasResolver  := alias.NewResolver(db, rdb)
	ppEngine       := promptpolicy.NewEngine(db, rdb, log)
	dockerDriver   := controller.NewDockerDriver()
	modelCtrl      := controller.NewModelController(db, rdb, dockerDriver, log)
	placementEng   := placement.NewEngine(db, log)
	svcRegistry    := services.NewRegistry(db, log)
	agentAuthSvc   := agentauth.NewService(db, cfg.Auth.JWTSecret)
	taskMgr        := taskmanager.NewManager(db, log)

	usageCtx, usageCancel := context.WithCancel(ctx)
	defer usageCancel()
	go usageTracker.StartConsumer(usageCtx)

	// Task timeout goroutine — marks stale tasks as timed-out every minute
	go func() {
		t := time.NewTicker(1 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-usageCtx.Done():
				return
			case <-t.C:
				if n, err := taskMgr.TimeoutStale(usageCtx); err == nil && n > 0 {
					log.Info("timed out stale tasks", zap.Int64("count", n))
				}
			}
		}
	}()

	// ── Node Agent (in-process for single-server deployment) ──────────────────
	// The in-process agent runs only when NEXUS_AGENT_ENABLED=true is set.
	// It self-registers using the real machine hostname (no hardcoded names).
	// In production, run the standalone nexus-nodeagent binary on each node instead.
	if os.Getenv("NEXUS_AGENT_ENABLED") == "true" {
		agentNodeID := startInProcessAgent(ctx, db, log, usageCtx)
		if agentNodeID != "" {
			log.Info("in-process node agent started", zap.String("node_id", agentNodeID))
		}
	}

	// ── Node Health Monitor ────────────────────────────────────────────────────
	// Watches heartbeat timestamps and transitions nodes ONLINE→UNHEALTHY→OFFLINE.
	// When a node goes OFFLINE, all its runtimes become LOST and endpoints are
	// removed from gateway routing.
	nodeMonitor := nodehealth.NewMonitor(db, log)
	go nodeMonitor.Start(usageCtx)
	log.Info("node health monitor started")

	// ── Preemption Engine ─────────────────────────────────────────────────────
	// Watches node pressure every 30s, evicts lower-priority runtimes when
	// VRAM/GPU/RAM thresholds are breached.
	preemptEngine := preemption.NewEngine(db, taskMgr, log)
	go preemptEngine.Start(usageCtx)
	log.Info("preemption engine started")

	// ── Project Metrics Collector ─────────────────────────────────────────────
	projectMetrics := project.NewMetricsCollector(db, log)
	go projectMetrics.Start(usageCtx)
	log.Info("project metrics collector started")

	// ── Handlers ──────────────────────────────────────────────────────────────
	orgH        := handlers.NewOrgHandler(db)
	teamH       := handlers.NewTeamHandler(db, rdb, policyEngine)
	apikeyH     := handlers.NewAPIKeyHandler(db, rdb)
	runtimeH    := handlers.NewRuntimeHandler(db, rdb, registry, modelCtrl).WithPlacement(placementEng).WithTaskManager(taskMgr)
	controllerH := handlers.NewControllerHandler(modelCtrl)
	gpuH        := handlers.NewGPUHandler(gpuInventory)
	usageH      := handlers.NewUsageHandler(usageTracker)
	aliasH      := handlers.NewAliasHandler(aliasResolver)
	ppH         := handlers.NewPromptPolicyHandler(ppEngine)
	serviceH    := handlers.NewServiceHandler(db, svcRegistry, placementEng, registry, modelCtrl)
	nodeH       := handlers.NewNodeHandler(db)
	placementH  := handlers.NewPlacementHandler(db, placementEng)
	agentH      := handlers.NewAgentHandler(db, agentAuthSvc, taskMgr)
	taskH       := handlers.NewTaskHandler(taskMgr)
	requireH    := handlers.NewRequirementsHandler(db)
	lazyH       := handlers.NewLazyRuntimeHandler(db)
	projectH    := handlers.NewProjectHandler(db)

	// ── Router ────────────────────────────────────────────────────────────────
	gin.SetMode(cfg.Server.Mode)
	r := gin.New()
	r.Use(gin.Recovery(), gin.Logger())
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	a := r.Group("/admin/v1")

	// ── Organizations ─────────────────────────────────────────────────────────
	a.POST("/orgs",       orgH.CreateOrg)
	a.GET("/orgs",        orgH.ListOrgs)
	a.GET("/orgs/:id",    orgH.GetOrg)
	a.DELETE("/orgs/:id", orgH.DeactivateOrg)

	// ── Teams (flat — no nesting under /orgs to avoid Gin wildcard conflicts) ─
	// Create: pass org_id in the request body.
	a.POST("/teams",          teamH.CreateTeam)    // body: {org_id, name, slug, priority}
	a.GET("/teams",           teamH.ListTeams)     // query: ?org_id=...
	a.GET("/teams/:id",       teamH.GetTeam)
	a.PUT("/teams/:id",       teamH.UpdateTeam)
	a.DELETE("/teams/:id",    teamH.DeactivateTeam)
	a.GET("/teams/:id/policy",           teamH.GetTeamPolicy)
	a.PUT("/teams/:id/policy",           teamH.UpdateTeamPolicy)
	a.GET("/teams/:id/models",           teamH.ListTeamModels)
	a.POST("/teams/:id/models",          teamH.AddModelPermission)
	a.DELETE("/teams/:id/models/:model", teamH.RemoveModelPermission)

	// ── API Keys ──────────────────────────────────────────────────────────────
	a.POST("/teams/:id/api-keys",   apikeyH.CreateAPIKey)
	a.GET("/teams/:id/api-keys",    apikeyH.ListAPIKeys)
	a.DELETE("/api-keys/:id",       apikeyH.RevokeAPIKey)

	// ── Models ────────────────────────────────────────────────────────────────
	// POST /admin/v1/models/deploy  ← must come before /models/:id to avoid conflict
	a.POST("/models/deploy",              runtimeH.DeployModel)
	a.POST("/models/import-ollama",       runtimeH.ImportOllamaModels)
	a.POST("/models",                     runtimeH.RegisterModel)
	a.GET("/models",                      runtimeH.ListModels)
	a.POST("/models/:id/endpoints",       runtimeH.AddEndpoint)
	a.DELETE("/models/:id/endpoints/:ep", runtimeH.RemoveEndpoint)
	a.POST("/models/:id/drain",           runtimeH.DrainModel)
	a.POST("/models/:id/enable",          runtimeH.EnableModel)
	a.POST("/models/:id/disable",         runtimeH.DisableModel)
	a.POST("/models/:id/reset-health",    runtimeH.ResetHealth)
	a.PUT("/models/:id/runtime-config",   runtimeH.UpdateRuntimeConfig)
	a.PUT("/models/:id/pool-strategy",    runtimeH.UpdatePoolStrategy)
	a.GET("/models/:id/health",           runtimeH.GetModelHealth)
	a.DELETE("/models/:id",               runtimeH.DeleteModel)
	a.GET("/models/:id/deploy-status",    runtimeH.GetDeployStatus)

	// ── Model Controller ──────────────────────────────────────────────────────
	a.POST("/models/:id/start",    controllerH.StartModel)
	a.POST("/models/:id/stop",     controllerH.StopModel)
	a.POST("/models/:id/restart",  controllerH.RestartModel)
	a.POST("/models/:id/upgrade",  controllerH.UpgradeModel)
	a.POST("/models/:id/rollback", controllerH.RollbackModel)
	a.GET("/models/:id/logs",      controllerH.GetModelLogs)

	// ── GPU Inventory ─────────────────────────────────────────────────────────
	a.POST("/gpu/nodes",                   gpuH.RegisterNode)
	a.GET("/gpu/nodes",                    gpuH.ListNodes)
	a.DELETE("/gpu/nodes/:id",             gpuH.DeleteGPUNode)
	a.POST("/gpu/nodes/:id/devices",       gpuH.RegisterDevice)
	a.GET("/gpu/nodes/:id/devices",        gpuH.ListDevices)
	a.DELETE("/gpu/nodes/:id/devices/:device_id", gpuH.DeleteGPUDevice)
	a.POST("/gpu/pack",                    gpuH.PackModels)

	// ── Usage & Billing ───────────────────────────────────────────────────────
	a.GET("/usage/teams/:id",              usageH.GetTeamUsage)
	a.GET("/usage/orgs/:id/monthly-spend", usageH.GetOrgSpend)
	a.POST("/usage/aggregate",             usageH.TriggerAggregation)

	// ── Model Aliases ─────────────────────────────────────────────────────────
	a.POST("/aliases",          aliasH.CreateAlias)
	a.DELETE("/aliases",        aliasH.DeleteAlias)
	a.GET("/aliases",           aliasH.ListAliases)
	a.GET("/aliases/resolve",   aliasH.ResolveAlias)

	// ── Prompt Policies ───────────────────────────────────────────────────────
	a.POST("/prompt-policies",  ppH.CreatePolicy)

	// ── AI Service Registry ───────────────────────────────────────────────────
	// POST /services/deploy must come before /services/:id to avoid Gin conflicts
	a.POST("/services/deploy",                serviceH.DeployService)
	a.POST("/services",                        serviceH.RegisterService)
	a.GET("/services",                         serviceH.ListServices)
	a.GET("/services/:id/reservation",         serviceH.GetReservation)
	a.PUT("/services/:id/reservation",         serviceH.UpsertReservation)

	// ── Cluster Nodes ─────────────────────────────────────────────────────────
	a.POST("/nodes",                     nodeH.RegisterNode)
	a.GET("/nodes",                      nodeH.ListNodes)
	a.GET("/nodes/:id",                  nodeH.GetNode)
	a.PUT("/nodes/:id/labels",           nodeH.UpdateLabels)
	a.POST("/nodes/:id/heartbeat",       nodeH.Heartbeat)
	a.POST("/nodes/:id/inventory",       nodeH.PushInventory)
	a.POST("/nodes/:id/telemetry",       nodeH.PushTelemetry)
	a.GET("/nodes/:id/telemetry",        nodeH.GetTelemetry)
	a.GET("/nodes/:id/inventory",        nodeH.GetInventory)
	a.POST("/nodes/:id/drain",           nodeH.DrainNode)
	a.DELETE("/nodes/:id",               nodeH.DeleteNode)
	a.GET("/nodes/:id/gpus",             nodeH.GetNodeGPUs)
	a.GET("/nodes/:id/health-events",    nodeH.GetNodeHealthEvents)

	// ── Model Lifecycle (archive/restore) ────────────────────────────────────
	a.POST("/models/:id/archive",           runtimeH.ArchiveModel)
	a.POST("/models/:id/restore",           runtimeH.RestoreModel)

	// ── Runtime Requirements ──────────────────────────────────────────────────
	a.POST("/models/:id/requirements",      requireH.UpsertRequirements)
	a.GET("/models/:id/requirements",       requireH.GetRequirements)
	a.GET("/scheduler/compatible-nodes",    requireH.CompatibleNodes)
	a.POST("/placement/simulate",           placementH.Simulate)
	a.GET("/placement/decisions",           placementH.ListDecisions)

	// ── Lazy-Load Runtime Config ──────────────────────────────────────────────
	// Configure per-model GGUF source, idle timeout, GPU layers, etc.
	a.PUT("/models/:id/lazy-config",        lazyH.SetLazyConfig)
	a.GET("/models/:id/lazy-config",        lazyH.GetLazyConfig)
	a.GET("/models/:id/runtime-status",     lazyH.GetRuntimeStatus)

	// ── Projects ──────────────────────────────────────────────────────────────
	a.POST("/projects",                         projectH.CreateProject)
	a.GET("/projects",                          projectH.ListProjects)
	a.GET("/projects/:id",                      projectH.GetProject)
	a.PUT("/projects/:id",                      projectH.UpdateProject)
	a.DELETE("/projects/:id",                   projectH.DeleteProject)
	a.POST("/projects/:id/reserve",             projectH.Reserve)
	a.POST("/projects/:id/priority",            projectH.ChangePriority)
	a.PUT("/projects/:id/protection",           projectH.SetProtection)
	a.GET("/projects/:id/runtimes",             projectH.GetRuntimes)
	a.GET("/projects/:id/usage",                projectH.GetUsage)
	a.GET("/projects/:id/preemptions",          projectH.GetPreemptions)
	a.GET("/projects/:id/queue",                projectH.GetQueue)

	// ── Agent API (called by node agents, not human operators) ────────────────
	// Registration does NOT require auth (bootstrapping)
	r.POST("/agent/v1/register", agentH.Register)

	// All other agent routes require a valid node JWT
	agent := r.Group("/agent/v1", agentH.AgentAuthMiddleware())
	{
		agent.POST("/heartbeat",              agentH.Heartbeat)
		agent.GET("/tasks/pending",           agentH.PollTasks)
		agent.POST("/tasks/:id/claim",        agentH.ClaimTask)
		agent.POST("/tasks/:id/running",      agentH.MarkTaskRunning)
		agent.POST("/tasks/:id/complete",     agentH.CompleteTask)
		agent.POST("/tasks/:id/fail",         agentH.FailTask)
		agent.POST("/inventory",              agentH.PushInventory)
		agent.POST("/telemetry",              agentH.PushTelemetry)
		agent.POST("/model-cache",            agentH.PushModelCache)
		agent.PUT("/runtimes/:id",            agentH.UpdateRuntime)
		agent.GET("/runtimes",                agentH.ListRuntimes)
	}

	// ── Task Management (admin operator API for dispatching tasks) ────────────
	a.POST("/nodes/:id/tasks",    taskH.DispatchTask)
	a.GET("/nodes/:id/tasks",     taskH.ListNodeTasks)
	a.GET("/tasks/:id",           taskH.GetTask)
	a.DELETE("/tasks/:id",        taskH.CancelTask)
	a.GET("/nodes/:id/runtimes",  taskH.ListNodeRuntimes)
	// Model cache (read by admin UI deploy form)
	a.GET("/nodes/:id/model-cache", agentH.GetNodeModelCache)

	// ── Metrics ───────────────────────────────────────────────────────────────
	// Admin uses port 9091 to avoid conflict with gateway's 9090
	adminMetricsPort := "9091"
	if p := os.Getenv("NEXUS_ADMIN_METRICS_PORT"); p != "" {
		adminMetricsPort = p
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsSrv := &http.Server{Addr: ":" + adminMetricsPort, Handler: metricsMux}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Warn("metrics server failed to start (non-fatal)", zap.Error(err))
		}
	}()

	// ── Main server ───────────────────────────────────────────────────────────
	adminPort := "8081"
	if p := os.Getenv("NEXUS_ADMIN_PORT"); p != "" {
		adminPort = p
	}

	srv := &http.Server{Addr: ":" + adminPort, Handler: r}

	// Start in goroutine so we can wait for signal
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
	_ = metricsSrv.Shutdown(shutCtx)
	log.Info("nexus-admin stopped")
}

// startInProcessAgent self-registers the current machine as a cluster node
// (using its real hostname) and starts the node agent loop.
// Used only when NEXUS_AGENT_ENABLED=true — intended for single-server dev setups.
func startInProcessAgent(ctx context.Context, db *sqlx.DB, log *zap.Logger, runCtx context.Context) string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		log.Warn("could not determine hostname for in-process agent", zap.Error(err))
		return ""
	}

	// Find existing node by hostname
	var nodeID string
	err = db.GetContext(ctx, &nodeID, `SELECT id FROM nodes WHERE hostname = $1 LIMIT 1`, hostname)
	if err != nil {
		// Auto-register this machine as a new node
		_, err2 := db.ExecContext(ctx, `
			INSERT INTO nodes (hostname, display_name, status, created_at, updated_at)
			VALUES ($1, $1, 'online', NOW(), NOW())
			ON CONFLICT (hostname) DO UPDATE SET updated_at = NOW()`, hostname)
		if err2 != nil {
			log.Warn("in-process agent: could not register node",
				zap.String("hostname", hostname), zap.Error(err2))
			return ""
		}
		// Re-fetch the ID
		if err3 := db.GetContext(ctx, &nodeID,
			`SELECT id FROM nodes WHERE hostname = $1 LIMIT 1`, hostname); err3 != nil {
			log.Warn("in-process agent: could not fetch registered node id", zap.Error(err3))
			return ""
		}
	}

	agent := nodeagent.NewAgent(db, nodeID, 30*time.Second, log)
	go agent.Start(runCtx)
	return nodeID
}