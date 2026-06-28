package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/nexusllm/nexusllm/internal/admin/handlers"
	"github.com/nexusllm/nexusllm/internal/agentauth"
	"github.com/nexusllm/nexusllm/internal/alias"
	"github.com/nexusllm/nexusllm/internal/config"
	"github.com/nexusllm/nexusllm/internal/controller"
	"github.com/nexusllm/nexusllm/internal/gpu"
	"github.com/nexusllm/nexusllm/internal/ha"
	"github.com/nexusllm/nexusllm/internal/nodeagent"
	"github.com/nexusllm/nexusllm/internal/nodehealth"
	"github.com/nexusllm/nexusllm/internal/placement"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/nexusllm/nexusllm/internal/preemption"
	"github.com/nexusllm/nexusllm/internal/project"
	"github.com/nexusllm/nexusllm/internal/promptpolicy"
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
	policyEngine := policy.NewEngine(rdb)
	gpuInventory := gpu.NewInventory(db, log)
	usageTracker := usage.NewTracker(db, rdb, log)
	aliasResolver := alias.NewResolver(db, rdb)
	ppEngine := promptpolicy.NewEngine(db, rdb, log)
	dockerDriver := controller.NewDockerDriver()
	modelCtrl := controller.NewModelController(db, rdb, dockerDriver, log)
	placementEng := placement.NewEngine(db, log)
	svcRegistry := services.NewRegistry(db, log)
	agentAuthSvc := agentauth.NewService(db, cfg.Auth.JWTSecret)
	taskMgr := taskmanager.NewManager(db, log)

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

	// ── HA Reconciler ─────────────────────────────────────────────────────────
	// Continuously compares desired replica count (model_replica_specs) vs actual
	// (agent_runtimes). Automatically triggers START_MODEL tasks for lost/missing
	// replicas. Respects placement_policy (spread|pack|anti_affinity).
	haReconciler := ha.NewReconciler(db, taskMgr, log)
	go haReconciler.Start(usageCtx)
	log.Info("HA reconciler started")

	// ── Stuck-runtime sweeper ─────────────────────────────────────────────────
	// Self-healing: finds runtimes stuck in loading_model/waiting_ready longer
	// than 3 minutes, marks them failed, and re-enqueues START_MODEL so the
	// model recovers without operator intervention.
	// This closes the gap when waitForReady is not in flight (e.g. gateway
	// restarted, or no active request is waiting on the model).
	go runStuckRuntimeSweeper(usageCtx, db, taskMgr, log)
	log.Info("stuck-runtime sweeper started")

	// ── Registry periodic reload ───────────────────────────────────────────────
	// Picks up new replicas started by the HA reconciler without waiting for an
	// explicit enableEndpoint() call. Reloads every 10 seconds.
	go registry.StartPeriodicReload(usageCtx, 10*time.Second)
	log.Info("registry periodic reload started")
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
	orgH := handlers.NewOrgHandler(db)
	teamH := handlers.NewTeamHandler(db, rdb, policyEngine)
	apikeyH := handlers.NewAPIKeyHandler(db, rdb)
	runtimeH := handlers.NewRuntimeHandler(db, rdb, registry, modelCtrl).WithPlacement(placementEng).WithTaskManager(taskMgr)
	controllerH := handlers.NewControllerHandler(modelCtrl)
	gpuH := handlers.NewGPUHandler(gpuInventory)
	usageH := handlers.NewUsageHandler(usageTracker)
	aliasH := handlers.NewAliasHandler(aliasResolver)
	ppH := handlers.NewPromptPolicyHandler(ppEngine)
	serviceH := handlers.NewServiceHandler(db, svcRegistry, placementEng, registry, modelCtrl)
	nodeH := handlers.NewNodeHandler(db)
	placementH := handlers.NewPlacementHandler(db, placementEng)
	agentH := handlers.NewAgentHandler(db, agentAuthSvc, taskMgr)
	taskH := handlers.NewTaskHandler(taskMgr)
	requireH := handlers.NewRequirementsHandler(db)
	lazyH := handlers.NewLazyRuntimeHandler(db)
	projectH := handlers.NewProjectHandler(db)
	haH := handlers.NewHAHandler(db)
	ppH2 := handlers.NewProjectPolicyHandler(db, rdb, policyEngine, usageTracker)

	// ── Router ────────────────────────────────────────────────────────────────
	gin.SetMode(cfg.Server.Mode)
	r := gin.New()
	r.Use(gin.Recovery(), gin.Logger())
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	a := r.Group("/admin/v1")

	// ── Organizations ─────────────────────────────────────────────────────────
	a.POST("/orgs", orgH.CreateOrg)
	a.GET("/orgs", orgH.ListOrgs)
	a.GET("/orgs/:id", orgH.GetOrg)
	a.DELETE("/orgs/:id", orgH.DeactivateOrg)

	// ── Teams (flat — no nesting under /orgs to avoid Gin wildcard conflicts) ─
	// Create: pass org_id in the request body.
	a.POST("/teams", teamH.CreateTeam) // body: {org_id, name, slug, priority}
	a.GET("/teams", teamH.ListTeams)   // query: ?org_id=...
	a.GET("/teams/:id", teamH.GetTeam)
	a.PUT("/teams/:id", teamH.UpdateTeam)
	a.DELETE("/teams/:id", teamH.DeactivateTeam)
	a.GET("/teams/:id/policy", teamH.GetTeamPolicy)
	a.PUT("/teams/:id/policy", teamH.UpdateTeamPolicy)
	a.GET("/teams/:id/models", teamH.ListTeamModels)
	a.POST("/teams/:id/models", teamH.AddModelPermission)
	a.DELETE("/teams/:id/models/:model", teamH.RemoveModelPermission)

	// ── API Keys ──────────────────────────────────────────────────────────────
	a.POST("/teams/:id/api-keys", apikeyH.CreateAPIKey)
	a.GET("/teams/:id/api-keys", apikeyH.ListAPIKeys)
	a.DELETE("/api-keys/:id", apikeyH.RevokeAPIKey)
	a.PUT("/api-keys/:id/project", apikeyH.SetKeyProject) // scope key to a project

	// ── Models ────────────────────────────────────────────────────────────────
	// POST /admin/v1/models/deploy  ← must come before /models/:id to avoid conflict
	a.POST("/models/deploy", runtimeH.DeployModel)
	a.POST("/models/import-ollama", runtimeH.ImportOllamaModels)
	a.POST("/models", runtimeH.RegisterModel)
	a.GET("/models", runtimeH.ListModels)
	a.POST("/models/:id/endpoints", runtimeH.AddEndpoint)
	a.DELETE("/models/:id/endpoints/:ep", runtimeH.RemoveEndpoint)
	a.POST("/models/:id/drain", runtimeH.DrainModel)
	a.POST("/models/:id/enable", runtimeH.EnableModel)
	a.POST("/models/:id/disable", runtimeH.DisableModel)
	a.POST("/models/:id/reset-health", runtimeH.ResetHealth)
	a.PUT("/models/:id/runtime-config", runtimeH.UpdateRuntimeConfig)
	a.PUT("/models/:id/pool-strategy", runtimeH.UpdatePoolStrategy)
	a.GET("/models/:id/health", runtimeH.GetModelHealth)
	a.DELETE("/models/:id", runtimeH.DeleteModel)
	a.GET("/models/:id/deploy-status", runtimeH.GetDeployStatus)

	// ── Model Controller ──────────────────────────────────────────────────────
	a.POST("/models/:id/start", controllerH.StartModel)
	a.POST("/models/:id/stop", controllerH.StopModel)
	a.POST("/models/:id/restart", controllerH.RestartModel)
	a.POST("/models/:id/upgrade", controllerH.UpgradeModel)
	a.POST("/models/:id/rollback", controllerH.RollbackModel)
	a.GET("/models/:id/logs", controllerH.GetModelLogs)

	// ── GPU Inventory ─────────────────────────────────────────────────────────
	a.POST("/gpu/nodes", gpuH.RegisterNode)
	a.GET("/gpu/nodes", gpuH.ListNodes)
	a.DELETE("/gpu/nodes/:id", gpuH.DeleteGPUNode)
	a.POST("/gpu/nodes/:id/devices", gpuH.RegisterDevice)
	a.GET("/gpu/nodes/:id/devices", gpuH.ListDevices)
	a.DELETE("/gpu/nodes/:id/devices/:device_id", gpuH.DeleteGPUDevice)
	a.POST("/gpu/pack", gpuH.PackModels)

	// ── Usage & Billing ───────────────────────────────────────────────────────
	a.GET("/usage/teams/:id", usageH.GetTeamUsage)
	a.GET("/usage/orgs/:id/monthly-spend", usageH.GetOrgSpend)
	a.POST("/usage/aggregate", usageH.TriggerAggregation)

	// ── Model Aliases ─────────────────────────────────────────────────────────
	a.POST("/aliases", aliasH.CreateAlias)
	a.DELETE("/aliases", aliasH.DeleteAlias)
	a.GET("/aliases", aliasH.ListAliases)
	a.GET("/aliases/resolve", aliasH.ResolveAlias)

	// ── Prompt Policies ───────────────────────────────────────────────────────
	a.POST("/prompt-policies", ppH.CreatePolicy)

	// ── AI Service Registry ───────────────────────────────────────────────────
	// POST /services/deploy must come before /services/:id to avoid Gin conflicts
	a.POST("/services/deploy", serviceH.DeployService)
	a.POST("/services", serviceH.RegisterService)
	a.GET("/services", serviceH.ListServices)
	a.GET("/services/:id/reservation", serviceH.GetReservation)
	a.PUT("/services/:id/reservation", serviceH.UpsertReservation)

	// ── Cluster Nodes ─────────────────────────────────────────────────────────
	a.POST("/nodes", nodeH.RegisterNode)
	a.GET("/nodes", nodeH.ListNodes)
	a.GET("/nodes/:id", nodeH.GetNode)
	a.PUT("/nodes/:id/labels", nodeH.UpdateLabels)
	a.POST("/nodes/:id/heartbeat", nodeH.Heartbeat)
	a.POST("/nodes/:id/inventory", nodeH.PushInventory)
	a.POST("/nodes/:id/telemetry", nodeH.PushTelemetry)
	a.GET("/nodes/:id/telemetry", nodeH.GetTelemetry)
	a.GET("/nodes/:id/inventory", nodeH.GetInventory)
	a.POST("/nodes/:id/drain", nodeH.DrainNode)
	a.POST("/nodes/:id/cordon", nodeH.CordonNode)
	a.POST("/nodes/:id/uncordon", nodeH.UncordonNode)
	a.DELETE("/nodes/:id", nodeH.DeleteNode)
	a.GET("/nodes/:id/gpus", nodeH.GetNodeGPUs)
	a.GET("/nodes/:id/health-events", nodeH.GetNodeHealthEvents)

	// ── Model Lifecycle (archive/restore) ────────────────────────────────────
	a.POST("/models/:id/archive", runtimeH.ArchiveModel)
	a.POST("/models/:id/restore", runtimeH.RestoreModel)

	// ── Runtime Requirements ──────────────────────────────────────────────────
	a.POST("/models/:id/requirements", requireH.UpsertRequirements)
	a.GET("/models/:id/requirements", requireH.GetRequirements)
	a.GET("/scheduler/compatible-nodes", requireH.CompatibleNodes)
	a.POST("/placement/simulate", placementH.Simulate)
	a.GET("/placement/decisions", placementH.ListDecisions)
	a.GET("/scheduler/priority-presets", projectH.GetPriorityPresets)
	a.GET("/scheduler/queue", projectH.SchedulerQueue)
	a.GET("/scheduler/decisions", projectH.SchedulerDecisions)

	// ── High Availability ──────────────────────────────────────────────────────
	a.GET("/ha/status", haH.GetClusterHAStatus)
	a.GET("/ha/status/:model_id", haH.GetModelHAStatus)
	a.PUT("/ha/models/:model_id/replicas", haH.SetReplicaSpec)
	a.GET("/ha/recovery-log", haH.GetRecoveryLog)
	a.GET("/ha/recovery-log/:model_id", haH.GetModelRecoveryLog)

	// ── Lazy-Load Runtime Config ──────────────────────────────────────────────
	// Configure per-model GGUF source, idle timeout, GPU layers, etc.
	a.PUT("/models/:id/lazy-config", lazyH.SetLazyConfig)
	a.GET("/models/:id/lazy-config", lazyH.GetLazyConfig)
	a.GET("/models/:id/runtime-status", lazyH.GetRuntimeStatus)

	// ── Projects ──────────────────────────────────────────────────────────────
	a.POST("/projects", projectH.CreateProject)
	a.GET("/projects", projectH.ListProjects)
	a.GET("/projects/:id", projectH.GetProject)
	a.PUT("/projects/:id", projectH.UpdateProject)
	a.DELETE("/projects/:id", projectH.DeleteProject)
	a.POST("/projects/:id/reserve", projectH.Reserve)
	a.POST("/projects/:id/priority", projectH.ChangePriority)
	a.PUT("/projects/:id/protection", projectH.SetProtection)
	a.GET("/projects/:id/runtimes", projectH.GetRuntimes)
	a.GET("/projects/:id/usage", projectH.GetUsage)
	a.GET("/projects/:id/preemptions", projectH.GetPreemptions)
	a.GET("/projects/:id/queue", projectH.GetQueue)
	// Project policy, quota and usage analytics (migration 023)
	a.GET("/projects/:id/policy", ppH2.GetPolicy)
	a.PUT("/projects/:id/policy", ppH2.UpdatePolicy)
	a.GET("/projects/:id/quota", ppH2.GetQuotaStatus)
	a.GET("/projects/:id/usage/daily", ppH2.GetDailyUsage)
	a.GET("/projects/:id/usage/summary", ppH2.GetUsageSummary)

	// ── Agent API (called by node agents, not human operators) ────────────────
	// Registration does NOT require auth (bootstrapping)
	r.POST("/agent/v1/register", agentH.Register)

	// All other agent routes require a valid node JWT
	agent := r.Group("/agent/v1", agentH.AgentAuthMiddleware())
	{
		agent.POST("/heartbeat", agentH.Heartbeat)
		agent.GET("/tasks/pending", agentH.PollTasks)
		agent.POST("/tasks/:id/claim", agentH.ClaimTask)
		agent.POST("/tasks/:id/running", agentH.MarkTaskRunning)
		agent.POST("/tasks/:id/complete", agentH.CompleteTask)
		agent.POST("/tasks/:id/fail", agentH.FailTask)
		agent.POST("/inventory", agentH.PushInventory)
		agent.POST("/telemetry", agentH.PushTelemetry)
		agent.POST("/model-cache", agentH.PushModelCache)
		agent.PUT("/runtimes/:id", agentH.UpdateRuntime)
		agent.GET("/runtimes", agentH.ListRuntimes)
	}

	// ── Task Management (admin operator API for dispatching tasks) ────────────
	a.POST("/nodes/:id/tasks", taskH.DispatchTask)
	a.GET("/nodes/:id/tasks", taskH.ListNodeTasks)
	a.GET("/tasks/:id", taskH.GetTask)
	a.DELETE("/tasks/:id", taskH.CancelTask)
	a.GET("/nodes/:id/runtimes", taskH.ListNodeRuntimes)
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

// ─────────────────────────────────────────────────────────────────────────────
// runStuckRuntimeSweeper — self-healing background loop
// ─────────────────────────────────────────────────────────────────────────────

// runStuckRuntimeSweeper periodically finds runtimes stuck in startup states
// (loading_model, waiting_ready, starting, pending) for longer than
// stuckThreshold and automatically:
//  1. Marks the old runtime row as "failed".
//  2. Enqueues a fresh START_MODEL task so the model self-heals.
//
// This covers the case where:
//   - The gateway restarted and waitForReady is no longer polling.
//   - The container started but never became healthy (OOM, bad image, etc.).
//   - The node agent crashed between reporting "loading_model" and "ready".
func runStuckRuntimeSweeper(ctx context.Context, db *sqlx.DB, taskMgr *taskmanager.Manager, log *zap.Logger) {
	const stuckThreshold = 3 * time.Minute
	const sweepInterval = 60 * time.Second

	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	// Run once on startup to clear any pre-existing stuck rows.
	sweepStuckRuntimes(ctx, db, taskMgr, log, stuckThreshold)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepStuckRuntimes(ctx, db, taskMgr, log, stuckThreshold)
		}
	}
}

func sweepStuckRuntimes(ctx context.Context, db *sqlx.DB, taskMgr *taskmanager.Manager, log *zap.Logger, threshold time.Duration) {
	type stuckRow struct {
		RuntimeID   string    `db:"id"`
		ModelID     string    `db:"model_id"`
		ModelName   string    `db:"model_name"`
		NodeID      string    `db:"node_id"`
		State       string    `db:"state"`
		Backend     string    `db:"backend"`
		RuntimeName string    `db:"runtime_name"`
		BindHost    string    `db:"bind_host"`
		BindPort    int       `db:"bind_port"`
		UpdatedAt   time.Time `db:"updated_at"`
	}

	var rows []stuckRow
	// The sweeper ONLY catches startup-stuck runtimes: rows that have been in a
	// transient startup state longer than the threshold.
	//
	// NEVER kill READY/ACTIVE/WARM/IDLE runtimes here — those are handled by:
	//   - runtime watcher (health check failures → StatusDown after 3 fails)
	//   - node health monitor (node offline → LOST)
	//   - reconciler (LOST/FAILED below desired_replicas → new replica)
	//
	// HA replicas created by the reconciler have endpoint_id=NULL. They must
	// NOT be swept just because a legacy model_endpoints row has is_enabled=FALSE.
	// Runtime liveness is determined by runtime.state + container health, not
	// by endpoint row existence.
	err := db.SelectContext(ctx, &rows, `
		-- Only startup-stuck runtimes: stuck in a transient state beyond threshold.
		-- READY/ACTIVE/WARM/IDLE runtimes are NEVER touched by this sweeper.
		SELECT ar.id, ar.model_id::text, m.name AS model_name,
		       ar.node_id::text, ar.state, ar.backend,
		       ar.runtime_name, ar.bind_host, ar.bind_port, ar.updated_at
		FROM agent_runtimes ar
		JOIN models m ON m.id = ar.model_id
		WHERE ar.state IN ('loading_model','waiting_ready','starting','pending','validating','downloading')
		  AND ar.updated_at < NOW() - ($1 || ' seconds')::interval
		  AND m.enabled = TRUE`,
		int(threshold.Seconds()),
	)
	if err != nil {
		log.Warn("stuck-runtime sweep: query failed", zap.Error(err))
		return
	}
	if len(rows) == 0 {
		return
	}

	log.Info("stuck-runtime sweep: found stuck runtimes", zap.Int("count", len(rows)))

	for _, row := range rows {
		age := time.Since(row.UpdatedAt)

		// Fetch extra context for diagnostics before taking any action.
		var containerID, endpointID string
		var nodeOnline bool
		_ = db.QueryRowContext(ctx,
			`SELECT COALESCE(container_id,''), COALESCE(endpoint_id::text,'')
			 FROM agent_runtimes WHERE id=$1`, row.RuntimeID,
		).Scan(&containerID, &endpointID)
		_ = db.QueryRowContext(ctx,
			`SELECT status IN ('online','degraded') FROM nodes WHERE id=$1`, row.NodeID,
		).Scan(&nodeOnline)

		log.Warn("stuck-runtime sweep: marking startup-stuck runtime failed",
			zap.String("runtime_id", row.RuntimeID),
			zap.String("model", row.ModelName),
			zap.String("state", row.State),
			zap.String("container_id", containerID),
			zap.String("endpoint_id", endpointID),
			zap.String("node", row.NodeID),
			zap.Bool("node_online", nodeOnline),
			zap.Duration("stuck_for", age),
		)

		// Step 1: mark the stuck row failed so the activator stops waiting.
		_, _ = db.ExecContext(ctx, `
			UPDATE agent_runtimes
			SET state     = 'failed',
			    error_msg = $2,
			    updated_at = NOW()
			WHERE id = $1
			  AND state IN ('loading_model','waiting_ready','starting','pending','validating','downloading')`,
			row.RuntimeID,
			fmt.Sprintf("auto-reset by stuck-runtime sweeper after %s in state %s", age.Round(time.Second), row.State),
		)

		// Step 2: load the model config needed to build a START_MODEL payload.
		var cfg struct {
			Image          string  `db:"image"`
			GGUFPath       string  `db:"gguf_path"`
			HFRepo         string  `db:"hf_repo"`
			HFFile         string  `db:"hf_file"`
			HFToken        string  `db:"hf_token"`
			ModelsVolume   string  `db:"models_volume"`
			CtxSize        int     `db:"ctx_size"`
			NGPULayers     int     `db:"n_gpu_layers"`
			ExecutionMode  string  `db:"execution_mode"`
			WorkloadPolicy string  `db:"workload_policy"`
			TensorParallel int     `db:"tensor_parallel"`
			GPUMemoryUtil  float64 `db:"gpu_memory_util"`
			MaxModelLen    int     `db:"max_model_len"`
			Dtype          string  `db:"dtype"`
			Quantization   string  `db:"quantization"`
		}
		cfgErr := db.GetContext(ctx, &cfg, `
			SELECT
			    COALESCE(me.runtime_image,   '')          AS image,
			    COALESCE(mrc.gguf_path,      '')          AS gguf_path,
			    COALESCE(mrc.hf_repo,        '')          AS hf_repo,
			    COALESCE(mrc.hf_file,        '')          AS hf_file,
			    COALESCE(mrc.hf_token,       '')          AS hf_token,
			    COALESCE(mrc.models_volume,  '')          AS models_volume,
			    COALESCE(mrc.ctx_size,       4096)        AS ctx_size,
			    COALESCE(mrc.n_gpu_layers,   0)           AS n_gpu_layers,
			    COALESCE(mrc.execution_mode, 'auto')      AS execution_mode,
			    COALESCE(mrc.workload_policy,'lazy_load') AS workload_policy,
			    COALESCE(mrc.tensor_parallel,1)           AS tensor_parallel,
			    COALESCE(mrc.gpu_memory_util,0.90)        AS gpu_memory_util,
			    COALESCE(mrc.max_model_len,  0)           AS max_model_len,
			    COALESCE(mrc.dtype,         'auto')       AS dtype,
			    COALESCE(mrc.quantization,  '')           AS quantization
			FROM models m
			LEFT JOIN model_endpoints me
			       ON me.model_id = m.id
			      AND me.lifecycle_state NOT IN ('deleted')
			LEFT JOIN model_runtime_configs mrc ON mrc.model_id = m.id
			WHERE m.id = $1 AND m.enabled = TRUE
			ORDER BY me.priority ASC
			LIMIT 1`, row.ModelID)

		if cfgErr != nil || cfg.Image == "" {
			log.Warn("stuck-runtime sweep: cannot load config — skipping re-enqueue",
				zap.String("model", row.ModelName),
				zap.Error(cfgErr),
			)
			continue
		}

		// Step 3: create a fresh runtime row and enqueue START_MODEL.
		newRuntimeID := uuid.New().String()
		suffix := strings.ReplaceAll(newRuntimeID, "-", "")[:6]
		sanitize := func(s string) string {
			return strings.Map(func(r rune) rune {
				if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
					return r
				}
				if r >= 'A' && r <= 'Z' {
					return r + 32
				}
				return '-'
			}, s)
		}
		containerName := fmt.Sprintf("nexus-%s-r-%s", sanitize(row.ModelName), suffix)

		if cfg.ModelsVolume == "" {
			cfg.ModelsVolume = "llamacpp_models"
		}
		if cfg.CtxSize == 0 {
			cfg.CtxSize = 4096
		}

		// Insert the new runtime row. The task FK requires it to exist first.
		_, insertErr := db.ExecContext(ctx, `
			INSERT INTO agent_runtimes
			  (id, node_id, endpoint_id, model_id, runtime_name, backend,
			   state, gpu_ids, bind_host, bind_port, cpu_affinity, numa_node,
			   requested_mode, effective_mode, workload_policy)
			SELECT $1, $2, me.id, $3, $4, $5,
			       'pending', '[]'::jsonb, me.host, me.port, '', -1,
			       $6, $6, $7
			FROM model_endpoints me
			WHERE me.model_id = $3
			  AND me.lifecycle_state NOT IN ('deleted')
			ORDER BY me.priority ASC
			LIMIT 1`,
			newRuntimeID, row.NodeID, row.ModelID, containerName, row.Backend,
			cfg.ExecutionMode, cfg.WorkloadPolicy,
		)
		if insertErr != nil {
			log.Warn("stuck-runtime sweep: failed to insert new runtime row",
				zap.String("model", row.ModelName),
				zap.Error(insertErr),
			)
			continue
		}

		payload := taskmanager.StartModelPayload{
			RuntimeID:      newRuntimeID,
			ModelID:        row.ModelID,
			RuntimeName:    containerName,
			Backend:        row.Backend,
			Image:          cfg.Image,
			ModelName:      row.ModelName,
			ServedAs:       row.ModelName,
			BindHost:       row.BindHost,
			BindPort:       row.BindPort,
			GGUFPath:       cfg.GGUFPath,
			HFRepo:         cfg.HFRepo,
			HFFile:         cfg.HFFile,
			HFToken:        cfg.HFToken,
			ModelsVolume:   cfg.ModelsVolume,
			CtxSize:        cfg.CtxSize,
			NGPULayers:     cfg.NGPULayers,
			TensorParallel: cfg.TensorParallel,
			GPUMemoryUtil:  cfg.GPUMemoryUtil,
			MaxModelLen:    cfg.MaxModelLen,
			Dtype:          cfg.Dtype,
			Quantization:   cfg.Quantization,
			ExecutionMode:  cfg.ExecutionMode,
			WorkloadPolicy: cfg.WorkloadPolicy,
			Env:            map[string]string{},
		}
		if cfg.HFToken != "" {
			payload.Env["HUGGING_FACE_HUB_TOKEN"] = cfg.HFToken
		}

		taskID, taskErr := taskMgr.Enqueue(ctx, row.NodeID,
			taskmanager.TaskStartModel, payload,
			taskmanager.WithPriority(70),
			taskmanager.WithActor("stuck-sweeper"),
			taskmanager.WithRuntimeID(newRuntimeID),
			taskmanager.WithIdempotencyKey(fmt.Sprintf("stuck:%s:%s", row.ModelID, newRuntimeID)),
		)
		if taskErr != nil {
			// Roll back the orphan runtime row.
			_, _ = db.ExecContext(ctx,
				`UPDATE agent_runtimes SET state='failed', error_msg='sweeper task enqueue failed' WHERE id=$1`,
				newRuntimeID)
			log.Warn("stuck-runtime sweep: failed to enqueue START_MODEL",
				zap.String("model", row.ModelName),
				zap.Error(taskErr),
			)
			continue
		}

		log.Info("stuck-runtime sweep: recovery task dispatched",
			zap.String("model", row.ModelName),
			zap.String("old_runtime", row.RuntimeID),
			zap.String("new_runtime", newRuntimeID),
			zap.String("task_id", taskID),
		)
	}
}
