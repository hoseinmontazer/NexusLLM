// nexus-nodeagent — Standalone Node Agent
//
// Run this binary on every server in the cluster. It will:
//   1. Auto-register the node with the NexusLLM control plane (no manual setup)
//   2. Discover and register GPU devices from nvidia-smi
//   3. Collect CPU / RAM / disk / GPU metrics continuously
//   4. Push telemetry and heartbeat to the Admin API
//
// Required environment variables:
//   NEXUS_ADMIN_URL   — URL of the nexus-admin server (e.g. http://10.0.0.1:8081)
//
// Optional:
//   NEXUS_NODE_ID           — skip auto-registration, use this node ID
//   NEXUS_AGENT_INTERVAL    — telemetry push interval (default: 30s)
//   NEXUS_HEARTBEAT_INTERVAL — heartbeat interval (default: 15s)
//
// The agent stores its node ID in /var/lib/nexus-agent/node-id after first
// registration so it survives restarts without re-registering.
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
	"syscall"
	"time"

	"go.uber.org/zap"
)

const (
	agentVersion       = "1.0.0"
	stateDir           = "/var/lib/nexus-agent"
	nodeIDFile         = "/var/lib/nexus-agent/node-id"
	defaultInterval    = 30 * time.Second
	defaultHeartbeat   = 15 * time.Second
	registerRetryDelay = 10 * time.Second
)

// ─────────────────────────────────────────────────────────────────────────────
// Main
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync()

	adminURL := getenv("NEXUS_ADMIN_URL", "http://localhost:8081")
	intervalStr := getenv("NEXUS_AGENT_INTERVAL", "30s")
	heartbeatStr := getenv("NEXUS_HEARTBEAT_INTERVAL", "15s")

	interval, err := time.ParseDuration(intervalStr)
	if err != nil {
		interval = defaultInterval
	}
	heartbeatInterval, err := time.ParseDuration(heartbeatStr)
	if err != nil {
		heartbeatInterval = defaultHeartbeat
	}

	agent := &Agent{
		adminURL:  adminURL,
		interval:  interval,
		heartbeat: heartbeatInterval,
		log:       log,
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}

	hostname, _ := os.Hostname()
	agent.hostname = hostname
	log.Info("nexus-nodeagent starting",
		zap.String("hostname", hostname),
		zap.String("admin_url", adminURL),
		zap.Duration("telemetry_interval", interval),
		zap.Duration("heartbeat_interval", heartbeatInterval),
		zap.String("version", agentVersion),
	)

	ctx, cancel := context.WithCancel(context.Background())
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// ── 1. Get or register node ───────────────────────────────────────────────
	nodeID := getenv("NEXUS_NODE_ID", "")
	if nodeID == "" {
		nodeID = agent.loadStoredNodeID()
	}
	if nodeID == "" {
		log.Info("node not registered — registering with control plane...")
		for {
			id, regErr := agent.register(ctx)
			if regErr == nil {
				nodeID = id
				agent.saveNodeID(id)
				log.Info("node registered", zap.String("node_id", id))
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
	log.Info("node identity confirmed", zap.String("node_id", nodeID))

	// ── 2. Push full inventory on startup ─────────────────────────────────────
	if err := agent.pushInventory(ctx); err != nil {
		log.Warn("inventory push failed (non-fatal)", zap.Error(err))
	}

	// ── 3. Run loops ──────────────────────────────────────────────────────────
	go agent.heartbeatLoop(ctx)

	// Push telemetry immediately before starting the tick loop
	agent.pushTelemetry(ctx)

	go agent.telemetryLoop(ctx)

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
	adminURL  string
	nodeID    string
	hostname  string
	interval  time.Duration
	heartbeat time.Duration
	log       *zap.Logger
	client    *http.Client
}

// ─── Registration ─────────────────────────────────────────────────────────────

// register auto-detects hardware and registers this node with the control plane.
// Returns the assigned node ID.
func (a *Agent) register(ctx context.Context) (string, error) {
	cpuCores := runtime.NumCPU()
	ramMB := a.readTotalRAMMB()
	gpus := a.querySMI()

	var totalVRAMMB int64
	for _, g := range gpus {
		totalVRAMMB += int64(g.VRAMMB)
	}

	body := map[string]interface{}{
		"hostname":       a.hostname,
		"display_name":   a.hostname,
		"total_cpu":      cpuCores,
		"total_ram_mb":   ramMB,
		"total_vram_mb":  totalVRAMMB,
		"labels": map[string]string{
			"agent_version": agentVersion,
			"os":            runtime.GOOS + "/" + runtime.GOARCH,
		},
	}

	var result struct {
		ID       string `json:"id"`
		Hostname string `json:"hostname"`
	}
	if err := a.post(ctx, "/admin/v1/nodes", body, &result); err != nil {
		// Node may already exist — try to find it by hostname
		existing, findErr := a.findNodeByHostname(ctx)
		if findErr != nil {
			return "", fmt.Errorf("register: %w (find existing: %v)", err, findErr)
		}
		a.log.Info("node already registered", zap.String("node_id", existing))
		return existing, nil
	}
	return result.ID, nil
}

// findNodeByHostname searches the nodes list for a matching hostname.
func (a *Agent) findNodeByHostname(ctx context.Context) (string, error) {
	var result struct {
		Data []struct {
			ID       string `json:"id"`
			Hostname string `json:"hostname"`
		} `json:"data"`
	}
	if err := a.get(ctx, "/admin/v1/nodes", &result); err != nil {
		return "", err
	}
	for _, n := range result.Data {
		if n.Hostname == a.hostname {
			return n.ID, nil
		}
	}
	return "", fmt.Errorf("hostname %q not found in nodes list", a.hostname)
}

// ─── Heartbeat loop ───────────────────────────────────────────────────────────

func (a *Agent) heartbeatLoop(ctx context.Context) {
	// Send one immediately
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
	if err := a.post(ctx, "/admin/v1/nodes/"+a.nodeID+"/heartbeat", body, nil); err != nil {
		a.log.Warn("heartbeat failed", zap.Error(err))
	} else {
		a.log.Debug("heartbeat sent")
	}
}

// ─── Telemetry loop ───────────────────────────────────────────────────────────

func (a *Agent) telemetryLoop(ctx context.Context) {
	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.collectAndPush(ctx)
		}
	}
}

func (a *Agent) collectAndPush(ctx context.Context) {
	// Re-push inventory (updates totals, GPU devices, stores snapshot)
	if err := a.pushInventory(ctx); err != nil {
		a.log.Warn("inventory push failed", zap.Error(err))
	}

	// Push telemetry snapshot directly into the admin's telemetry endpoint
	a.pushTelemetry(ctx)
}

// pushTelemetry posts a single telemetry snapshot to the node telemetry endpoint.
func (a *Agent) pushTelemetry(ctx context.Context) {
	cpuUtil := a.readCPUUtil()
	ramTotal := a.readTotalRAMMB()
	ramUsed, ramAvail := a.readRAMUsage()
	diskTotal, diskUsed := a.readDiskInfo()
	numaNodes := a.readNUMANodes()
	gpus := a.querySMI()

	body := map[string]interface{}{
		"cpu_cores_total": runtime.NumCPU(),
		"cpu_util_pct":    cpuUtil,
		"ram_total_mb":    ramTotal,
		"ram_used_mb":     ramUsed,
		"ram_avail_mb":    ramAvail,
		"numa_nodes":      numaNodes,
		"disk_total_gb":   diskTotal,
		"disk_used_gb":    diskUsed,
		"gpus":            len(gpus),
	}

	if err := a.post(ctx, "/admin/v1/nodes/"+a.nodeID+"/telemetry", body, nil); err != nil {
		a.log.Debug("telemetry push failed", zap.Error(err))
	} else {
		a.log.Debug("telemetry pushed",
			zap.Float64("cpu_util_pct", cpuUtil),
			zap.Int64("ram_used_mb", ramUsed),
		)
	}
}

// ─── Inventory push ───────────────────────────────────────────────────────────

// pushInventory sends the full hardware inventory to the control plane.
// This updates node totals, registers GPU devices, and stores a snapshot.
func (a *Agent) pushInventory(ctx context.Context) error {
	cpuModel := a.readCPUModel()
	cpuCores := runtime.NumCPU()
	ramMB := a.readTotalRAMMB()
	ramUsedMB, ramAvailMB := a.readRAMUsage()
	cpuUtil := a.readCPUUtil()
	numaNodes := a.readNUMANodes()
	diskTotal, diskUsed := a.readDiskInfo()
	kernelVer := a.readKernelVersion()
	gpus := a.querySMI()

	// Build GPU JSON array
	gpuList := make([]map[string]interface{}, len(gpus))
	for i, g := range gpus {
		gpuList[i] = map[string]interface{}{
			"index":       g.DeviceIndex,
			"name":        g.Name,
			"vram_mb":     g.VRAMMB,
			"mem_used_mb": g.MemUsedMB,
			"util_pct":    g.UtilizationPct,
			"temp_c":      g.TemperatureC,
			"power_w":     g.PowerDrawW,
			"power_limit": g.PowerLimitW,
			"fan_pct":     g.FanSpeedPct,
			"pcie_bus_id": g.PCIeBusID,
			"numa_node":   g.NUMANode,
		}
	}

	var totalVRAMMB int64
	for _, g := range gpus {
		totalVRAMMB += int64(g.VRAMMB)
	}

	snapshot := map[string]interface{}{
		"hostname":      a.hostname,
		"agent_version": agentVersion,
		"cpu_model":     cpuModel,
		"cpu_cores":     cpuCores,
		"cpu_util_pct":  cpuUtil,
		"ram_total_mb":  ramMB,
		"ram_used_mb":   ramUsedMB,
		"ram_avail_mb":  ramAvailMB,
		"numa_nodes":    numaNodes,
		"disk_total_gb": diskTotal,
		"disk_used_gb":  diskUsed,
		"total_vram_mb": totalVRAMMB,
		"os":            runtime.GOOS + "/" + runtime.GOARCH,
		"kernel":        kernelVer,
		"gpus":          gpuList,
	}

	if err := a.post(ctx, "/admin/v1/nodes/"+a.nodeID+"/inventory", snapshot, nil); err != nil {
		return fmt.Errorf("push inventory: %w", err)
	}

	// Also register/update GPU devices in the GPU inventory
	a.syncGPUDevices(ctx, gpus)

	a.log.Info("inventory pushed",
		zap.Int("cpus", cpuCores),
		zap.Float64("cpu_util_pct", cpuUtil),
		zap.Int64("ram_mb", ramMB),
		zap.Int("gpus", len(gpus)),
		zap.Int64("total_vram_mb", totalVRAMMB),
	)
	return nil
}

// syncGPUDevices ensures GPU devices are registered in the GPU inventory
// and updates their live telemetry.
func (a *Agent) syncGPUDevices(ctx context.Context, gpus []GPUMetrics) {
	if len(gpus) == 0 {
		return
	}

	// Get existing gpu_nodes for this cluster node
	var gpuNodes struct {
		Data []struct {
			ID     string `json:"id"`
			NodeID string `json:"node_id"`
			Name   string `json:"name"`
		} `json:"data"`
	}
	if err := a.get(ctx, "/admin/v1/gpu/nodes?cluster_node_id="+a.nodeID, &gpuNodes); err != nil {
		a.log.Debug("could not list gpu nodes", zap.Error(err))
		return
	}

	// Find the gpu_node associated with this cluster node
	var gpuNodeID string
	for _, gn := range gpuNodes.Data {
		if gn.NodeID == a.nodeID || gn.ID != "" && gpuNodeID == "" {
			gpuNodeID = gn.ID
			break
		}
	}
	// If no gpu_node exists yet, create one
	if gpuNodeID == "" {
		var totalVRAMMB int64
		for _, g := range gpus {
			totalVRAMMB += int64(g.VRAMMB)
		}
		var result struct {
			ID string `json:"id"`
		}
		err := a.post(ctx, "/admin/v1/gpu/nodes", map[string]interface{}{
			"name":          a.hostname + "-gpu",
			"host":          a.hostname,
			"driver_type":   "docker",
			"total_vram_mb": totalVRAMMB,
			"node_id":       a.nodeID,
		}, &result)
		if err != nil {
			a.log.Debug("could not create gpu node", zap.Error(err))
			return
		}
		gpuNodeID = result.ID
		a.log.Info("gpu node created", zap.String("gpu_node_id", gpuNodeID))
	}

	// Get existing devices for this gpu_node
	var devices struct {
		Data []struct {
			ID          string `json:"id"`
			DeviceIndex int    `json:"device_index"`
		} `json:"data"`
	}
	_ = a.get(ctx, "/admin/v1/gpu/nodes/"+gpuNodeID+"/devices", &devices)

	existingIdx := make(map[int]bool)
	for _, d := range devices.Data {
		existingIdx[d.DeviceIndex] = true
	}

	// Register any new GPU devices
	for _, g := range gpus {
		if !existingIdx[g.DeviceIndex] {
			body := map[string]interface{}{
				"device_index": g.DeviceIndex,
				"name":         g.Name,
				"vram_mb":      g.VRAMMB,
			}
			if err := a.post(ctx, "/admin/v1/gpu/nodes/"+gpuNodeID+"/devices", body, nil); err == nil {
				a.log.Info("GPU device registered",
					zap.Int("index", g.DeviceIndex),
					zap.String("name", g.Name),
					zap.Int("vram_mb", g.VRAMMB),
				)
			}
		}
	}
}

// ─── Hardware collection ──────────────────────────────────────────────────────

type GPUMetrics struct {
	DeviceIndex    int
	Name           string
	VRAMMB         int
	MemUsedMB      int
	UtilizationPct int
	TemperatureC   int
	PowerDrawW     int
	PowerLimitW    int
	FanSpeedPct    int
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
		parts := strings.Fields(sc.Text())
		if len(parts) >= 2 && strings.TrimSuffix(parts[0], ":") == "MemTotal" {
			v, _ := strconv.ParseInt(parts[1], 10, 64)
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
		parts := strings.Fields(sc.Text())
		if len(parts) >= 2 {
			vals[strings.TrimSuffix(parts[0], ":")] = func() int64 {
				v, _ := strconv.ParseInt(parts[1], 10, 64)
				return v
			}()
		}
	}
	totalMB := vals["MemTotal"] / 1024
	availMBVal := vals["MemAvailable"] / 1024
	return totalMB - availMBVal, availMBVal
}

func (a *Agent) readCPUUtil() float64 {
	s1, err := a.readCPUStat()
	if err != nil {
		return 0
	}
	time.Sleep(200 * time.Millisecond)
	s2, err := a.readCPUStat()
	if err != nil {
		return 0
	}
	dTotal := float64(s2.total - s1.total)
	dIdle := float64(s2.idle - s1.idle)
	if dTotal == 0 {
		return 0
	}
	return (1 - dIdle/dTotal) * 100
}

type cpuStat struct{ total, idle int64 }

func (a *Agent) readCPUStat() (cpuStat, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuStat{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			break
		}
		var vals [10]int64
		for i := 1; i < len(fields) && i <= 10; i++ {
			vals[i-1], _ = strconv.ParseInt(fields[i], 10, 64)
		}
		idle := vals[3] + vals[4]
		var total int64
		for _, v := range vals {
			total += v
		}
		return cpuStat{total: total, idle: idle}, nil
	}
	return cpuStat{}, fmt.Errorf("cpu stat not found")
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
			fields := strings.Fields(sc.Text())
			if len(fields) >= 2 {
				n, _ := strconv.Atoi(fields[1])
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
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
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

func (a *Agent) querySMI() []GPUMetrics {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=index,name,memory.total,memory.used,utilization.gpu,"+
			"temperature.gpu,power.draw,power.limit,fan.speed,pci.bus_id",
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return nil // no GPU or no nvidia-smi
	}

	var gpus []GPUMetrics
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Split(line, ", ")
		if len(fields) < 10 {
			continue
		}
		g := GPUMetrics{}
		g.DeviceIndex, _ = strconv.Atoi(strings.TrimSpace(fields[0]))
		g.Name = strings.TrimSpace(fields[1])
		g.VRAMMB, _ = strconv.Atoi(strings.TrimSpace(fields[2]))
		g.MemUsedMB, _ = strconv.Atoi(strings.TrimSpace(fields[3]))
		g.UtilizationPct, _ = strconv.Atoi(strings.TrimSpace(fields[4]))
		g.TemperatureC, _ = strconv.Atoi(strings.TrimSpace(fields[5]))
		p, _ := strconv.ParseFloat(strings.TrimSpace(fields[6]), 64)
		g.PowerDrawW = int(p)
		l, _ := strconv.ParseFloat(strings.TrimSpace(fields[7]), 64)
		g.PowerLimitW = int(l)
		g.FanSpeedPct, _ = strconv.Atoi(strings.TrimSpace(fields[8]))
		g.PCIeBusID = strings.TrimSpace(fields[9])
		g.NUMANode = a.gpuNUMANode(g.PCIeBusID)
		gpus = append(gpus, g)
	}
	return gpus
}

func (a *Agent) gpuNUMANode(pcieID string) int {
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

// ─── Node ID persistence ──────────────────────────────────────────────────────

func (a *Agent) loadStoredNodeID() string {
	data, err := os.ReadFile(nodeIDFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (a *Agent) saveNodeID(id string) {
	_ = os.MkdirAll(filepath.Dir(nodeIDFile), 0755)
	_ = os.WriteFile(nodeIDFile, []byte(id), 0644)
}

// ─── HTTP helpers ─────────────────────────────────────────────────────────────

func (a *Agent) post(ctx context.Context, path string, body interface{}, out interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.adminURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s → HTTP %d: %s", path, resp.StatusCode, string(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (a *Agent) get(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.adminURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s → HTTP %d: %s", path, resp.StatusCode, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
