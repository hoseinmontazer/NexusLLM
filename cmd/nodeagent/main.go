// nexus-nodeagent — Production Node Agent v2
//
// Architecture: Control Plane is the brain. Node Agent is the executor.
//
// The agent:
//  1. Registers with the control plane (auto-discovers hardware)
//  2. Receives a JWT token for authenticated communication
//  3. Sends heartbeat + telemetry every 30s
//  4. Long-polls for tasks — claims, executes, reports back
//  5. NEVER makes placement or scheduling decisions
//
// Required:
//
//	NEXUS_ADMIN_URL  — URL of the nexus-admin control plane
//
// Optional:
//
//	NEXUS_NODE_ID            — skip re-registration, use stored/provided node ID
//	NEXUS_AGENT_TOKEN        — use this token (skip re-registration)
//	NEXUS_AGENT_INTERVAL     — telemetry push interval (default: 30s)
//	NEXUS_HEARTBEAT_INTERVAL — heartbeat interval (default: 15s)
//	NEXUS_TASK_WORKERS       — concurrent task executors (default: 4)
//	NEXUS_LOG_LEVEL          — debug | info | warn (default: info)
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nexusllm/nexusllm/internal/nodeagent"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	agentVersion       = "2.0.0"
	stateDir           = "/var/lib/nexus-agent"
	nodeIDFile         = "/var/lib/nexus-agent/node-id"
	tokenFile          = "/var/lib/nexus-agent/token"
	registerRetryDelay = 10 * time.Second
	taskPollWait       = 25 // seconds for long-poll
)

func main() {
	log := buildLogger(getenv("NEXUS_LOG_LEVEL", "info"))
	defer log.Sync()

	adminURL := getenv("NEXUS_ADMIN_URL", "http://localhost:8081")
	interval, _ := time.ParseDuration(getenv("NEXUS_AGENT_INTERVAL", "30s"))
	if interval == 0 {
		interval = 30 * time.Second
	}
	heartbeatInterval, _ := time.ParseDuration(getenv("NEXUS_HEARTBEAT_INTERVAL", "15s"))
	if heartbeatInterval == 0 {
		heartbeatInterval = 15 * time.Second
	}
	taskWorkers := 4
	if n, err := strconv.Atoi(getenv("NEXUS_TASK_WORKERS", "4")); err == nil && n > 0 {
		taskWorkers = n
	}

	agent := &Agent{
		adminURL:    adminURL,
		interval:    interval,
		heartbeat:   heartbeatInterval,
		taskWorkers: taskWorkers,
		log:         log,
		executor:    nodeagent.NewExecutor(log),
		reregister:  make(chan struct{}, 1),
		client: &http.Client{
			Timeout: 35 * time.Second, // longer than long-poll wait
		},
	}

	hostname, _ := os.Hostname()
	agent.hostname = hostname

	log.Info("nexus-nodeagent starting",
		zap.String("version", agentVersion),
		zap.String("hostname", hostname),
		zap.String("admin_url", adminURL),
		zap.Duration("telemetry_interval", interval),
		zap.Duration("heartbeat_interval", heartbeatInterval),
		zap.Int("task_workers", taskWorkers),
	)

	ctx, cancel := context.WithCancel(context.Background())
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// ── 1. Register and get token ─────────────────────────────────────────────
	nodeID := getenv("NEXUS_NODE_ID", loadFile(nodeIDFile))
	token := getenv("NEXUS_AGENT_TOKEN", loadFile(tokenFile))

	if nodeID == "" || token == "" {
		log.Info("registering with control plane...")
		for {
			id, tok, regErr := agent.register(ctx)
			if regErr == nil {
				nodeID = id
				token = tok
				saveFile(nodeIDFile, id)
				saveFile(tokenFile, tok)
				log.Info("registration complete",
					zap.String("node_id", id),
				)
				break
			}
			log.Warn("registration failed, retrying",
				zap.Error(regErr),
				zap.Duration("retry_in", registerRetryDelay),
			)
			select {
			case <-ctx.Done():
				return
			case <-time.After(registerRetryDelay):
			}
		}
	}

	agent.nodeID = nodeID
	agent.token = token
	log.Info("node identity confirmed", zap.String("node_id", nodeID))

	// ── Prometheus metrics ────────────────────────────────────────────────────
	metrics := nodeagent.NewAgentMetrics(nodeID, hostname)
	agent.metrics = metrics
	metricsAddr := getenv("NEXUS_AGENT_METRICS_ADDR", ":9092")
	go nodeagent.StartMetricsServer(metricsAddr)
	log.Info("agent metrics listening", zap.String("addr", metricsAddr))

	// ── 2. Initial inventory push ─────────────────────────────────────────────
	if err := agent.pushInventory(ctx); err != nil {
		log.Warn("initial inventory push failed (non-fatal)", zap.Error(err))
	}
	agent.pushTelemetry(ctx)

	// ── 3. Start background loops ─────────────────────────────────────────────
	go agent.heartbeatLoop(ctx)
	go agent.telemetryLoop(ctx)
	go agent.taskPollLoop(ctx)

	// ── 4. Auto re-register on 401 ────────────────────────────────────────────
	// When any authenticated request receives HTTP 401, doRequest signals the
	// reregister channel. This goroutine detects it, deletes the stale token
	// file, re-registers, and updates the in-memory token — no restart needed.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-agent.reregister:
				log.Warn("received 401 — token invalid, re-registering with control plane")
				// Delete stale files so the next register call gets a fresh token.
				_ = os.Remove(tokenFile)
				_ = os.Remove(nodeIDFile)

				for {
					id, tok, regErr := agent.register(ctx)
					if regErr == nil {
						agent.nodeID = id
						agent.token = tok
						saveFile(nodeIDFile, id)
						saveFile(tokenFile, tok)
						log.Info("re-registration complete", zap.String("node_id", id))
						break
					}
					log.Warn("re-registration failed, retrying",
						zap.Error(regErr),
						zap.Duration("retry_in", registerRetryDelay),
					)
					select {
					case <-ctx.Done():
						return
					case <-time.After(registerRetryDelay):
					}
				}
			}
		}
	}()

	log.Info("nexus-nodeagent running — waiting for tasks")
	<-quit
	log.Info("nexus-nodeagent shutting down...")
	cancel()
	time.Sleep(500 * time.Millisecond)
	log.Info("nexus-nodeagent stopped")
}

// ─────────────────────────────────────────────────────────────────────────────
// Agent
// ─────────────────────────────────────────────────────────────────────────────

type Agent struct {
	adminURL    string
	nodeID      string
	hostname    string
	token       string
	interval    time.Duration
	heartbeat   time.Duration
	taskWorkers int
	log         *zap.Logger
	executor    *nodeagent.Executor
	metrics     *nodeagent.AgentMetrics
	client      *http.Client
	// reregister is signalled when a 401 Unauthorized is received, indicating
	// the stored token is no longer valid. The main loop re-registers and updates
	// the token without requiring a process restart.
	reregister chan struct{}
}

// ─── Registration ─────────────────────────────────────────────────────────────

func (a *Agent) register(ctx context.Context) (nodeID, token string, err error) {
	cpuCores := runtime.NumCPU()
	ramMB := a.readTotalRAMMB()
	gpus := a.querySMI()

	var totalVRAMMB int64
	for _, g := range gpus {
		totalVRAMMB += int64(g.VRAMMB)
	}

	capabilities := map[string]interface{}{
		"docker":    a.hasDocker(),
		"vllm":      false, // will be confirmed by successful task
		"ollama":    a.hasOllama(),
		"tgi":       false,
		"whisper":   false,
		"tts":       false,
		"embedding": false,
		"gpu":       len(gpus) > 0,
		"gpu_count": len(gpus),
	}

	body := map[string]interface{}{
		"hostname":      a.hostname,
		"ip_address":    a.localIP(),
		"total_cpu":     cpuCores,
		"total_ram_mb":  ramMB,
		"total_vram_mb": totalVRAMMB,
		"agent_version": agentVersion,
		"capabilities":  capabilities,
		"labels": map[string]string{
			"os":            runtime.GOOS + "/" + runtime.GOARCH,
			"agent_version": agentVersion,
		},
	}

	var result struct {
		NodeID  string `json:"node_id"`
		Token   string `json:"token"`
		Message string `json:"message"`
	}
	// Register does NOT need auth token (bootstrapping)
	if err := a.postNoAuth(ctx, "/agent/v1/register", body, &result); err != nil {
		return "", "", err
	}
	if result.NodeID == "" || result.Token == "" {
		return "", "", fmt.Errorf("registration response missing node_id or token")
	}
	return result.NodeID, result.Token, nil
}

// ─── Heartbeat ────────────────────────────────────────────────────────────────

func (a *Agent) heartbeatLoop(ctx context.Context) {
	a.sendHeartbeat(ctx)
	t := time.NewTicker(a.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.sendHeartbeat(ctx)
		}
	}
}

func (a *Agent) sendHeartbeat(ctx context.Context) {
	body := map[string]interface{}{
		"agent_version": agentVersion,
		"status":        "online",
	}
	if err := a.post(ctx, "/agent/v1/heartbeat", body, nil); err != nil {
		a.log.Warn("heartbeat failed", zap.Error(err))
	}
}

// ─── Telemetry ────────────────────────────────────────────────────────────────

func (a *Agent) telemetryLoop(ctx context.Context) {
	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.pushInventory(ctx); err != nil {
				a.log.Warn("inventory push failed", zap.Error(err))
			}
			a.pushTelemetry(ctx)
			a.pushModelCache(ctx)
		}
	}
}

// pushModelCache scans local HF and Ollama caches and reports them to the control plane.
func (a *Agent) pushModelCache(ctx context.Context) {
	models := nodeagent.ScanModelCache()
	if len(models) == 0 {
		return
	}
	items := make([]map[string]interface{}, len(models))
	for i, m := range models {
		items[i] = map[string]interface{}{
			"model_ref":  m.ModelRef,
			"backend":    m.Backend,
			"size_bytes": m.SizeBytes,
			"is_cached":  m.IsCached,
		}
	}
	if err := a.post(ctx, "/agent/v1/model-cache", map[string]interface{}{"models": items}, nil); err != nil {
		a.log.Debug("model cache push failed", zap.Error(err))
	} else {
		a.log.Debug("model cache pushed", zap.Int("count", len(models)))
	}
}

func (a *Agent) pushTelemetry(ctx context.Context) {
	cpuUtil := a.readCPUUtil()
	ramTotal := a.readTotalRAMMB()
	ramUsed, ramAvail := a.readRAMUsage()
	diskTotal, diskUsed := a.readDiskInfo()
	numaNodes := a.readNUMANodes()
	gpus := a.querySMI()

	gpuList := make([]map[string]interface{}, len(gpus))
	for i, g := range gpus {
		gpuList[i] = map[string]interface{}{
			"index": g.DeviceIndex, "name": g.Name,
			"vram_mb": g.VRAMMB, "mem_used_mb": g.MemUsedMB,
			"util_pct": g.UtilizationPct, "temp_c": g.TemperatureC,
			"power_w": g.PowerDrawW,
		}
	}

	body := map[string]interface{}{
		"cpu_cores_total": runtime.NumCPU(),
		"cpu_util_pct":    cpuUtil,
		"ram_total_mb":    ramTotal,
		"ram_used_mb":     ramUsed,
		"ram_avail_mb":    ramAvail,
		"numa_nodes":      numaNodes,
		"disk_total_gb":   diskTotal,
		"disk_used_gb":    diskUsed,
		"gpus":            gpuList,
	}

	if err := a.post(ctx, "/agent/v1/telemetry", body, nil); err != nil {
		a.log.Debug("telemetry push failed", zap.Error(err))
	} else {
		a.log.Debug("telemetry pushed",
			zap.Float64("cpu_pct", cpuUtil),
			zap.Int64("ram_used_mb", ramUsed),
		)
	}

	// Update Prometheus metrics
	if a.metrics != nil {
		a.metrics.CPUUsage.With(prometheus.Labels{}).Set(cpuUtil)
		a.metrics.MemoryUsage.With(prometheus.Labels{}).Set(float64(ramUsed))
		a.metrics.MemoryTotal.With(prometheus.Labels{}).Set(float64(ramTotal))
		for _, g := range gpus {
			idxStr := strconv.Itoa(g.DeviceIndex)
			lbls := prometheus.Labels{"device_index": idxStr, "device_name": g.Name}
			a.metrics.GPUMemoryUsed.With(lbls).Set(float64(g.MemUsedMB))
			a.metrics.GPUMemoryTotal.With(lbls).Set(float64(g.VRAMMB))
			a.metrics.GPUUtilization.With(lbls).Set(float64(g.UtilizationPct))
			a.metrics.GPUTemperature.With(lbls).Set(float64(g.TemperatureC))
			a.metrics.GPUPowerDraw.With(lbls).Set(float64(g.PowerDrawW))
		}
	}
}

func (a *Agent) pushInventory(ctx context.Context) error {
	cpuModel := a.readCPUModel()
	gpus := a.querySMI()
	var totalVRAMMB int64
	for _, g := range gpus {
		totalVRAMMB += int64(g.VRAMMB)
	}

	gpuList := make([]map[string]interface{}, len(gpus))
	for i, g := range gpus {
		gpuList[i] = map[string]interface{}{
			"index": g.DeviceIndex, "name": g.Name, "vram_mb": g.VRAMMB,
			"pcie_bus_id": g.PCIeBusID, "numa_node": g.NUMANode,
		}
	}

	diskTotal, diskUsed := a.readDiskInfo()
	snapshot := map[string]interface{}{
		"hostname": a.hostname, "agent_version": agentVersion,
		"cpu_model": cpuModel, "cpu_cores": runtime.NumCPU(),
		"ram_total_mb": a.readTotalRAMMB(), "numa_nodes": a.readNUMANodes(),
		"disk_total_gb": diskTotal, "disk_used_gb": diskUsed,
		"total_vram_mb": totalVRAMMB, "os": runtime.GOOS + "/" + runtime.GOARCH,
		"kernel": a.readKernelVersion(), "gpus": gpuList,
	}

	return a.post(ctx, "/agent/v1/inventory", snapshot, nil)
}

// ─── Task poll loop ───────────────────────────────────────────────────────────

func (a *Agent) taskPollLoop(ctx context.Context) {
	// Semaphore to limit concurrent task executions
	sem := make(chan struct{}, a.taskWorkers)
	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		default:
		}

		tasks, err := a.pollTasks(ctx)
		if err != nil {
			a.log.Warn("task poll failed", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		for _, task := range tasks {
			// Claim the task before executing
			ok, claimErr := a.claimTask(ctx, task.ID)
			if claimErr != nil || !ok {
				continue // already claimed by another worker
			}

			// Execute concurrently up to taskWorkers
			sem <- struct{}{}
			wg.Add(1)
			go func(t nodeagent.RemoteTask) {
				defer func() { <-sem; wg.Done() }()
				a.executeTask(ctx, t)
			}(task)
		}

		// If no tasks, the long-poll already waited; if tasks found, loop immediately
	}
}

func (a *Agent) pollTasks(ctx context.Context) ([]nodeagent.RemoteTask, error) {
	url := fmt.Sprintf("/agent/v1/tasks/pending?wait=%d&limit=5", taskPollWait)
	var result struct {
		Tasks []nodeagent.RemoteTask `json:"tasks"`
		Count int                    `json:"count"`
	}
	if err := a.getAuth(ctx, url, &result); err != nil {
		return nil, err
	}
	return result.Tasks, nil
}

func (a *Agent) claimTask(ctx context.Context, taskID string) (bool, error) {
	var result struct {
		Claimed bool `json:"claimed"`
	}
	err := a.post(ctx, "/agent/v1/tasks/"+taskID+"/claim", map[string]interface{}{}, &result)
	if err != nil {
		return false, err
	}
	return result.Claimed, nil
}

func (a *Agent) executeTask(ctx context.Context, task nodeagent.RemoteTask) {
	a.log.Info("executing task",
		zap.String("task_id", task.ID),
		zap.String("task_type", string(task.TaskType)),
	)

	start := time.Now()

	// Mark running
	_ = a.post(ctx, "/agent/v1/tasks/"+task.ID+"/running", map[string]interface{}{}, nil)

	// Touch the runtime row immediately so the gateway knows work is in progress
	// and doesn't treat it as a stale loading row.
	var runtimeID string
	if err := a.getAuth(ctx, "/agent/v1/tasks/"+task.ID+"/runtime-id", &struct {
		RuntimeID string `json:"runtime_id"`
	}{}); err == nil {
		// best-effort — extract runtime_id from task payload instead
	}
	// Parse runtime_id from task payload directly
	var payloadFields struct {
		RuntimeID string `json:"runtime_id"`
	}
	if len(task.Payload) > 0 {
		_ = json.Unmarshal(task.Payload, &payloadFields)
		runtimeID = payloadFields.RuntimeID
	}
	if runtimeID != "" {
		_ = a.put(ctx, "/agent/v1/runtimes/"+runtimeID, map[string]interface{}{
			"state": "loading",
		}, nil)
	}

	// Execute
	result := a.executor.Execute(ctx, task)
	elapsed := time.Since(start)

	// Record Prometheus metrics
	if a.metrics != nil {
		taskType := string(task.TaskType)
		status := "success"
		if !result.Success {
			status = "failed"
		}
		a.metrics.TasksTotal.With(prometheus.Labels{
			"task_type": taskType, "status": status,
		}).Inc()
		a.metrics.TaskDuration.With(prometheus.Labels{
			"task_type": taskType,
		}).Observe(elapsed.Seconds())
	}

	// Report result
	if result.Success {
		resultMap := map[string]interface{}{
			"runtime_id":    result.RuntimeID,
			"runtime_state": result.RuntimeState,
			"container_id":  result.ContainerID,
		}
		for k, v := range result.Data {
			resultMap[k] = v
		}
		if err := a.post(ctx, "/agent/v1/tasks/"+task.ID+"/complete", resultMap, nil); err != nil {
			a.log.Warn("failed to report task completion", zap.Error(err))
		}
		if result.RuntimeID != "" && result.RuntimeState != "" {
			_ = a.put(ctx, "/agent/v1/runtimes/"+result.RuntimeID, map[string]interface{}{
				"state":        result.RuntimeState,
				"container_id": result.ContainerID,
			}, nil)
		}
	} else {
		if err := a.post(ctx, "/agent/v1/tasks/"+task.ID+"/fail",
			map[string]interface{}{
				"error":         result.Error,
				"runtime_id":    result.RuntimeID,
				"runtime_state": result.RuntimeState,
			}, nil); err != nil {
			a.log.Warn("failed to report task failure", zap.Error(err))
		}
		if result.RuntimeID != "" {
			_ = a.put(ctx, "/agent/v1/runtimes/"+result.RuntimeID, map[string]interface{}{
				"state": func() string {
					if result.RuntimeState != "" {
						return result.RuntimeState
					}
					return "failed"
				}(),
			}, nil)
		}
	}
}

// ─── Hardware collection ──────────────────────────────────────────────────────

type gpuInfo struct {
	DeviceIndex    int
	Name           string
	VRAMMB         int
	MemUsedMB      int
	UtilizationPct int
	TemperatureC   int
	PowerDrawW     int
	PCIeBusID      string
	NUMANode       int
}

func (a *Agent) readTotalRAMMB() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) >= 2 && strings.TrimSuffix(p[0], ":") == "MemTotal" {
			v, _ := strconv.ParseInt(p[1], 10, 64)
			return v / 1024
		}
	}
	return 0
}

func (a *Agent) readRAMUsage() (usedMB, availMB int64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	vals := make(map[string]int64)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) >= 2 {
			v, _ := strconv.ParseInt(p[1], 10, 64)
			vals[strings.TrimSuffix(p[0], ":")] = v
		}
	}
	totalMB := vals["MemTotal"] / 1024
	avail := vals["MemAvailable"] / 1024
	return totalMB - avail, avail
}

func (a *Agent) readCPUUtil() float64 {
	type stat struct{ total, idle int64 }
	read := func() (stat, error) {
		f, err := os.Open("/proc/stat")
		if err != nil {
			return stat{}, err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "cpu ") {
				continue
			}
			fields := strings.Fields(line)
			var vals [10]int64
			for i := 1; i < len(fields) && i <= 10; i++ {
				vals[i-1], _ = strconv.ParseInt(fields[i], 10, 64)
			}
			idle := vals[3] + vals[4]
			var total int64
			for _, v := range vals {
				total += v
			}
			return stat{total, idle}, nil
		}
		return stat{}, fmt.Errorf("not found")
	}
	s1, err := read()
	if err != nil {
		return 0
	}
	time.Sleep(200 * time.Millisecond)
	s2, err := read()
	if err != nil {
		return 0
	}
	dt := float64(s2.total - s1.total)
	di := float64(s2.idle - s1.idle)
	if dt == 0 {
		return 0
	}
	return (1 - di/dt) * 100
}

func (a *Agent) readDiskInfo() (totalGB, usedGB int64) {
	out, err := exec.Command("df", "-BG", "/").Output()
	if err != nil {
		return 0, 0
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) < 2 {
		return 0, 0
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 4 {
		return 0, 0
	}
	t, _ := strconv.ParseInt(strings.TrimSuffix(fields[1], "G"), 10, 64)
	u, _ := strconv.ParseInt(strings.TrimSuffix(fields[2], "G"), 10, 64)
	return t, u
}

func (a *Agent) readNUMANodes() int {
	out, err := exec.Command("numactl", "--hardware").Output()
	if err != nil {
		return 1
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "available:") {
			f := strings.Fields(sc.Text())
			if len(f) >= 2 {
				n, _ := strconv.Atoi(f[1])
				return n
			}
		}
	}
	return 1
}

func (a *Agent) readCPUModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "model name") {
			p := strings.SplitN(line, ":", 2)
			if len(p) == 2 {
				return strings.TrimSpace(p[1])
			}
		}
	}
	return "unknown"
}

func (a *Agent) readKernelVersion() string {
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func (a *Agent) querySMI() []gpuInfo {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=index,name,memory.total,memory.used,utilization.gpu,temperature.gpu,power.draw,pci.bus_id",
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return nil
	}
	var gpus []gpuInfo
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Split(line, ", ")
		if len(fields) < 8 {
			continue
		}
		g := gpuInfo{}
		g.DeviceIndex, _ = strconv.Atoi(strings.TrimSpace(fields[0]))
		g.Name = strings.TrimSpace(fields[1])
		g.VRAMMB, _ = strconv.Atoi(strings.TrimSpace(fields[2]))
		g.MemUsedMB, _ = strconv.Atoi(strings.TrimSpace(fields[3]))
		g.UtilizationPct, _ = strconv.Atoi(strings.TrimSpace(fields[4]))
		g.TemperatureC, _ = strconv.Atoi(strings.TrimSpace(fields[5]))
		p, _ := strconv.ParseFloat(strings.TrimSpace(fields[6]), 64)
		g.PowerDrawW = int(p)
		g.PCIeBusID = strings.TrimSpace(fields[7])
		g.NUMANode = gpuNUMANode(g.PCIeBusID)
		gpus = append(gpus, g)
	}
	return gpus
}

func gpuNUMANode(pcieID string) int {
	if pcieID == "" {
		return 0
	}
	id := strings.ToLower(strings.TrimPrefix(pcieID, "00000000:"))
	data, err := os.ReadFile(fmt.Sprintf("/sys/bus/pci/devices/%s/numa_node", id))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if n < 0 {
		return 0
	}
	return n
}

func (a *Agent) hasDocker() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

func (a *Agent) hasOllama() bool {
	_, err := exec.LookPath("ollama")
	return err == nil
}

func (a *Agent) localIP() string {
	out, err := exec.Command("hostname", "-I").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// ─── HTTP helpers ─────────────────────────────────────────────────────────────

func (a *Agent) post(ctx context.Context, path string, body interface{}, out interface{}) error {
	return a.doRequest(ctx, http.MethodPost, path, body, out, true)
}

func (a *Agent) postNoAuth(ctx context.Context, path string, body interface{}, out interface{}) error {
	return a.doRequest(ctx, http.MethodPost, path, body, out, false)
}

func (a *Agent) getAuth(ctx context.Context, path string, out interface{}) error {
	return a.doRequest(ctx, http.MethodGet, path, nil, out, true)
}

func (a *Agent) put(ctx context.Context, path string, body interface{}, out interface{}) error {
	return a.doRequest(ctx, http.MethodPut, path, body, out, true)
}

func (a *Agent) doRequest(ctx context.Context, method, path string, body interface{}, out interface{}, auth bool) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, a.adminURL+path, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth && a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		// On 401, signal the main loop to re-register with fresh credentials.
		// Non-blocking send: if the channel already has a pending signal, skip.
		if resp.StatusCode == http.StatusUnauthorized && auth && a.reregister != nil {
			select {
			case a.reregister <- struct{}{}:
			default:
			}
		}
		return fmt.Errorf("%s %s → HTTP %d: %s", method, path, resp.StatusCode, string(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ─── Persistence ─────────────────────────────────────────────────────────────

func loadFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveFile(path, content string) {
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	_ = os.WriteFile(path, []byte(content), 0600)
}

// ─── Logger ───────────────────────────────────────────────────────────────────

func buildLogger(level string) *zap.Logger {
	lvl := zapcore.InfoLevel
	switch level {
	case "debug":
		lvl = zapcore.DebugLevel
	case "warn":
		lvl = zapcore.WarnLevel
	}
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(lvl)
	l, _ := cfg.Build()
	return l
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
