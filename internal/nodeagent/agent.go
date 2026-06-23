// Package nodeagent implements the Node Agent — a lightweight background
// process that collects hardware metrics (CPU, RAM, GPU, NUMA) and reports
// them to the NexusLLM control plane via the Admin API heartbeat endpoint.
//
// Architecture: Node Agent → Nexus Control Plane (Admin API).
//
// In a single-server deployment the agent runs in-process alongside the
// admin server. In a future multi-server deployment it can be compiled as a
// standalone binary and run on each node, pushing data to the central admin.
package nodeagent

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
)

// cryptoRandRead is a var so tests can substitute it.
var cryptoRandRead = rand.Read

// ─────────────────────────────────────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────────────────────────────────────

// HardwareMetrics is a point-in-time snapshot of node hardware state.
type HardwareMetrics struct {
	// Node identity
	NodeID   string
	Hostname string

	// CPU
	CPUCoresTotal int
	CPUCoresUsed  int
	CPUUtilPct    float64

	// RAM
	RAMTotalMB int64
	RAMUsedMB  int64
	RAMAvailMB int64

	// NUMA topology
	NUMANodes    int
	NUMATopology map[int]NUMANodeInfo

	// Disk
	DiskTotalGB int64
	DiskUsedGB  int64

	// Network (optional — set if readable)
	NetRxMbps float64
	NetTxMbps float64

	CollectedAt time.Time
}

// NUMANodeInfo describes one NUMA node's resources.
type NUMANodeInfo struct {
	Node   int
	CPUs   []int
	MemMB  int64
	FreeMB int64
}

// GPUMetrics is a snapshot of a single GPU device's state.
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

// InventorySnapshot is the full hardware inventory of a node.
type InventorySnapshot struct {
	Hostname     string
	AgentVersion string
	CPUModel     string
	CPUCores     int
	RAMTotalMB   int64
	GPUs         []GPUMetrics
	NUMANodes    int
	OSInfo       string
	KernelVer    string
	CollectedAt  time.Time
}

// ─────────────────────────────────────────────────────────────────────────────
// Agent
// ─────────────────────────────────────────────────────────────────────────────

const agentVersion = "1.0.0"

// Agent collects metrics and reports to the control plane.
type Agent struct {
	db       *sqlx.DB
	nodeID   string
	hostname string
	interval time.Duration
	log      *zap.Logger
}

// NewAgent constructs a Node Agent.
func NewAgent(db *sqlx.DB, nodeID string, interval time.Duration, log *zap.Logger) *Agent {
	hostname, _ := os.Hostname()
	return &Agent{
		db:       db,
		nodeID:   nodeID,
		hostname: hostname,
		interval: interval,
		log:      log,
	}
}

// Start begins the agent collection loop. Blocks until ctx is cancelled.
func (a *Agent) Start(ctx context.Context) {
	// Run an inventory snapshot on startup
	a.reportInventory(ctx)

	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.collectAndPersist(ctx)
		}
	}
}

// ─── Heartbeat + inventory ────────────────────────────────────────────────────

// Heartbeat updates the node's last_heartbeat_at and status in the DB.
func (a *Agent) Heartbeat(ctx context.Context) {
	_, _ = a.db.ExecContext(ctx, `
		UPDATE nodes
		SET status = 'online', last_heartbeat_at = NOW(), agent_version = $1, updated_at = NOW()
		WHERE id = $2`,
		agentVersion, a.nodeID,
	)
}

func (a *Agent) reportInventory(ctx context.Context) {
	snap := a.CollectInventory()

	// Update node totals from fresh inventory
	_, _ = a.db.ExecContext(ctx, `
		UPDATE nodes
		SET total_cpu = $1, total_ram_mb = $2,
		    agent_version = $3, last_heartbeat_at = NOW(),
		    status = 'online', updated_at = NOW()
		WHERE id = $4`,
		snap.CPUCores, snap.RAMTotalMB, agentVersion, a.nodeID,
	)

	// Persist snapshot for history
	_, _ = a.db.ExecContext(ctx, `
		INSERT INTO node_inventory_snapshots (id, node_id, snapshot, agent_ver, reported_at)
		VALUES (gen_random_uuid(), $1, $2, $3, NOW())`,
		a.nodeID,
		inventoryToJSON(snap),
		agentVersion,
	)

	// Sync GPU device telemetry
	a.syncGPUDevices(ctx, snap.GPUs)

	// If no GPUs found, explicitly mark this node as CPU-only in node_capabilities.
	// This ensures resolveExecutionMode returns "cpu" for "auto" mode on this node.
	if len(snap.GPUs) == 0 {
		_, _ = a.db.ExecContext(ctx, `
			INSERT INTO node_capabilities (node_id, has_gpu, gpu_count, gpu_available, gpu_vram_mb, updated_at)
			VALUES ($1, FALSE, 0, FALSE, 0, NOW())
			ON CONFLICT (node_id) DO UPDATE SET
			  has_gpu       = FALSE,
			  gpu_count     = 0,
			  gpu_available = FALSE,
			  gpu_vram_mb   = 0,
			  updated_at    = NOW()`,
			a.nodeID,
		)
	}

	a.log.Info("inventory reported",
		zap.String("node", a.hostname),
		zap.Int("cpus", snap.CPUCores),
		zap.Int64("ram_mb", snap.RAMTotalMB),
		zap.Int("gpus", len(snap.GPUs)),
	)
}

func (a *Agent) collectAndPersist(ctx context.Context) {
	a.Heartbeat(ctx)

	hw := a.CollectHardware()
	a.persistNodeTelemetry(ctx, hw)

	gpus := a.CollectGPUMetrics()
	a.persistGPUTelemetry(ctx, gpus)
}

// ─── Hardware collection ──────────────────────────────────────────────────────

// CollectHardware returns a hardware metrics snapshot using /proc on Linux.
// Falls back gracefully on non-Linux platforms.
func (a *Agent) CollectHardware() HardwareMetrics {
	m := HardwareMetrics{
		NodeID:       a.nodeID,
		Hostname:     a.hostname,
		CollectedAt:  time.Now(),
		NUMATopology: make(map[int]NUMANodeInfo),
	}

	m.CPUCoresTotal = runtime.NumCPU()
	m.CPUUtilPct = a.readCPUUtil()

	if ram, used, avail, err := a.readMemInfo(); err == nil {
		m.RAMTotalMB = ram
		m.RAMUsedMB = used
		m.RAMAvailMB = avail
	}

	if total, used, err := a.readDiskInfo(); err == nil {
		m.DiskTotalGB = total
		m.DiskUsedGB = used
	}

	m.NUMANodes = a.readNUMANodes()
	return m
}

// CollectGPUMetrics collects per-GPU metrics via nvidia-smi.
func (a *Agent) CollectGPUMetrics() []GPUMetrics {
	return a.querySMI()
}

// CollectInventory performs a full hardware inventory scan.
func (a *Agent) CollectInventory() InventorySnapshot {
	snap := InventorySnapshot{
		Hostname:     a.hostname,
		AgentVersion: agentVersion,
		CPUCores:     runtime.NumCPU(),
		CollectedAt:  time.Now(),
	}
	if ram, _, _, err := a.readMemInfo(); err == nil {
		snap.RAMTotalMB = ram
	}
	snap.GPUs = a.querySMI()
	snap.NUMANodes = a.readNUMANodes()
	snap.CPUModel = a.readCPUModel()
	snap.OSInfo = runtime.GOOS + "/" + runtime.GOARCH
	snap.KernelVer = a.readKernelVersion()
	return snap
}

// ─── /proc readers ────────────────────────────────────────────────────────────

func (a *Agent) readMemInfo() (totalMB, usedMB, availMB int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()

	vals := make(map[string]int64)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		key := strings.TrimSuffix(parts[0], ":")
		v, _ := strconv.ParseInt(parts[1], 10, 64)
		vals[key] = v
	}
	totalMB = vals["MemTotal"] / 1024
	availMB = vals["MemAvailable"] / 1024
	usedMB = totalMB - availMB
	return totalMB, usedMB, availMB, nil
}

func (a *Agent) readCPUUtil() float64 {
	// Read two snapshots 200ms apart and compute delta
	s1, err1 := a.readCPUStat()
	if err1 != nil {
		return 0
	}
	time.Sleep(200 * time.Millisecond)
	s2, err2 := a.readCPUStat()
	if err2 != nil {
		return 0
	}

	deltaTotal := float64(s2.total - s1.total)
	deltaIdle := float64(s2.idle - s1.idle)
	if deltaTotal == 0 {
		return 0
	}
	return (1.0 - deltaIdle/deltaTotal) * 100.0
}

type cpuStat struct{ total, idle int64 }

func (a *Agent) readCPUStat() (cpuStat, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuStat{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
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
		// user, nice, system, idle, iowait, irq, softirq, steal, guest, guest_nice
		idle := vals[3] + vals[4] // idle + iowait
		total := int64(0)
		for _, v := range vals {
			total += v
		}
		return cpuStat{total: total, idle: idle}, nil
	}
	return cpuStat{}, fmt.Errorf("cpu stat not found")
}

func (a *Agent) readDiskInfo() (totalGB, usedGB int64, err error) {
	out, err := exec.Command("df", "-BG", "/").Output()
	if err != nil {
		return 0, 0, err
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) < 2 {
		return 0, 0, fmt.Errorf("unexpected df output")
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 4 {
		return 0, 0, fmt.Errorf("unexpected df fields")
	}
	total, _ := strconv.ParseInt(strings.TrimSuffix(fields[1], "G"), 10, 64)
	used, _ := strconv.ParseInt(strings.TrimSuffix(fields[2], "G"), 10, 64)
	return total, used, nil
}

func (a *Agent) readNUMANodes() int {
	out, err := exec.Command("numactl", "--hardware").Output()
	if err != nil {
		return 1 // assume single-NUMA on failure
	}
	// Parse "available: N nodes (0-N)"
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "available:") {
			fields := strings.Fields(line)
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
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
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

// ─── nvidia-smi ──────────────────────────────────────────────────────────────

// querySMI queries nvidia-smi for per-GPU metrics.
// Returns empty slice if nvidia-smi is not available (graceful degradation).
func (a *Agent) querySMI() []GPUMetrics {
	// Query: index, name, memory.total, memory.used, utilization.gpu,
	//        temperature.gpu, power.draw, power.limit, fan.speed, pci.bus_id
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=index,name,memory.total,memory.used,utilization.gpu,"+
			"temperature.gpu,power.draw,power.limit,fan.speed,pci.bus_id",
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		// nvidia-smi not available (e.g. CPU-only dev machine)
		return []GPUMetrics{}
	}

	var gpus []GPUMetrics
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
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
		power, _ := strconv.ParseFloat(strings.TrimSpace(fields[6]), 64)
		g.PowerDrawW = int(power)
		limit, _ := strconv.ParseFloat(strings.TrimSpace(fields[7]), 64)
		g.PowerLimitW = int(limit)
		fan, _ := strconv.Atoi(strings.TrimSpace(fields[8]))
		g.FanSpeedPct = fan
		g.PCIeBusID = strings.TrimSpace(fields[9])
		g.NUMANode = a.gpuNUMANode(g.PCIeBusID)
		gpus = append(gpus, g)
	}
	return gpus
}

// gpuNUMANode reads the NUMA node affinity for a GPU from sysfs.
func (a *Agent) gpuNUMANode(pcieID string) int {
	// sysfs path: /sys/bus/pci/devices/<pcie_id>/numa_node
	if pcieID == "" {
		return 0
	}
	id := strings.ToLower(strings.TrimPrefix(pcieID, "00000000:"))
	path := fmt.Sprintf("/sys/bus/pci/devices/%s/numa_node", id)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if n < 0 {
		return 0
	}
	return n
}

// ─── DB persistence ───────────────────────────────────────────────────────────

func (a *Agent) persistNodeTelemetry(ctx context.Context, m HardwareMetrics) {
	_, _ = a.db.ExecContext(ctx, `
		INSERT INTO node_telemetry
		  (node_id, cpu_cores_total, cpu_util_pct,
		   ram_total_mb, ram_used_mb, ram_avail_mb,
		   numa_nodes, disk_total_gb, disk_used_gb, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		a.nodeID,
		m.CPUCoresTotal, m.CPUUtilPct,
		m.RAMTotalMB, m.RAMUsedMB, m.RAMAvailMB,
		m.NUMANodes,
		m.DiskTotalGB, m.DiskUsedGB,
		m.CollectedAt,
	)
}

func (a *Agent) persistGPUTelemetry(ctx context.Context, gpus []GPUMetrics) {
	for _, g := range gpus {
		// Look up device ID by device index + node
		var deviceID string
		err := a.db.GetContext(ctx, &deviceID, `
			SELECT d.id FROM gpu_devices d
			JOIN gpu_nodes gn ON gn.id = d.node_id
			WHERE gn.node_id = $1 AND d.device_index = $2
			LIMIT 1`,
			a.nodeID, g.DeviceIndex,
		)
		if err != nil {
			continue
		}

		// Update live telemetry on device row
		_, _ = a.db.ExecContext(ctx, `
			UPDATE gpu_devices
			SET utilization_pct = $1, temperature_c = $2, power_draw_w = $3,
			    last_seen_at = NOW(), updated_at = NOW()
			WHERE id = $4`,
			g.UtilizationPct, g.TemperatureC, g.PowerDrawW, deviceID,
		)

		// Insert into time-series table
		_, _ = a.db.ExecContext(ctx, `
			INSERT INTO gpu_telemetry
			  (device_id, utilization_pct, memory_used_mb, memory_total_mb,
			   temperature_c, power_draw_w, power_limit_w, fan_speed_pct, recorded_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NOW())`,
			deviceID,
			g.UtilizationPct, g.MemUsedMB, g.VRAMMB,
			g.TemperatureC, g.PowerDrawW, g.PowerLimitW, g.FanSpeedPct,
		)
	}
}

func (a *Agent) syncGPUDevices(ctx context.Context, gpus []GPUMetrics) {
	if len(gpus) == 0 {
		return
	}

	// Ensure a gpu_nodes row exists for this cluster node
	var gpuNodeID string
	err := a.db.GetContext(ctx, &gpuNodeID,
		`SELECT id FROM gpu_nodes WHERE node_id = $1 LIMIT 1`, a.nodeID)
	if err != nil {
		// Auto-create the gpu_nodes entry
		gpuNodeID = newUUID()
		hostname, _ := os.Hostname()
		_, _ = a.db.ExecContext(ctx, `
			INSERT INTO gpu_nodes (id, name, host, driver_type, total_vram_mb, is_available, node_id)
			VALUES ($1,$2,$3,'nvidia',0,TRUE,$4)
			ON CONFLICT DO NOTHING`,
			gpuNodeID, hostname+"-gpus", hostname, a.nodeID,
		)
		// Re-fetch in case ON CONFLICT DO NOTHING fired
		_ = a.db.GetContext(ctx, &gpuNodeID,
			`SELECT id FROM gpu_nodes WHERE node_id = $1 LIMIT 1`, a.nodeID)
	}
	if gpuNodeID == "" {
		return
	}

	// Total VRAM across all GPUs
	var totalVRAM int64
	for _, g := range gpus {
		totalVRAM += int64(g.VRAMMB)
	}
	_, _ = a.db.ExecContext(ctx,
		`UPDATE gpu_nodes SET total_vram_mb=$1, updated_at=NOW() WHERE id=$2`,
		totalVRAM, gpuNodeID)

	// Update nodes.total_vram_mb
	_, _ = a.db.ExecContext(ctx,
		`UPDATE nodes SET total_vram_mb=$1, updated_at=NOW() WHERE id=$2`,
		totalVRAM, a.nodeID)

	for _, g := range gpus {
		// Upsert device row
		_, _ = a.db.ExecContext(ctx, `
			INSERT INTO gpu_devices
			  (id, node_id, device_index, name, vram_mb, status,
			   utilization_pct, temperature_c, power_draw_w,
			   pcie_bus_id, numa_node, last_seen_at, created_at, updated_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, 'available', $5, $6, $7, $8, $9, NOW(), NOW(), NOW())
			ON CONFLICT (node_id, device_index) DO UPDATE SET
			  name            = EXCLUDED.name,
			  vram_mb         = EXCLUDED.vram_mb,
			  utilization_pct = EXCLUDED.utilization_pct,
			  temperature_c   = EXCLUDED.temperature_c,
			  power_draw_w    = EXCLUDED.power_draw_w,
			  pcie_bus_id     = EXCLUDED.pcie_bus_id,
			  numa_node       = EXCLUDED.numa_node,
			  last_seen_at    = NOW(),
			  updated_at      = NOW()`,
			gpuNodeID, g.DeviceIndex, g.Name, g.VRAMMB,
			g.UtilizationPct, g.TemperatureC, g.PowerDrawW,
			g.PCIeBusID, g.NUMANode,
		)
	}

	a.log.Info("GPU devices synced",
		zap.String("node", a.hostname),
		zap.Int("gpu_count", len(gpus)),
		zap.Int64("total_vram_mb", totalVRAM),
	)

	// Update node_capabilities so the activator's resolveExecutionMode query
	// can determine gpu_available without joining gpu_devices.
	_, _ = a.db.ExecContext(ctx, `
		INSERT INTO node_capabilities (node_id, has_gpu, gpu_count, gpu_available, gpu_vram_mb, updated_at)
		VALUES ($1, TRUE, $2, TRUE, $3, NOW())
		ON CONFLICT (node_id) DO UPDATE SET
		  has_gpu       = TRUE,
		  gpu_count     = EXCLUDED.gpu_count,
		  gpu_available = TRUE,
		  gpu_vram_mb   = EXCLUDED.gpu_vram_mb,
		  updated_at    = NOW()`,
		a.nodeID, len(gpus), totalVRAM,
	)
}

// newUUID generates a new random UUID string using crypto/rand.
func newUUID() string {
	b := make([]byte, 16)
	_, _ = cryptoRandRead(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ─── JSON helper ──────────────────────────────────────────────────────────────

func inventoryToJSON(snap InventorySnapshot) string {
	gpusJSON := "["
	for i, g := range snap.GPUs {
		if i > 0 {
			gpusJSON += ","
		}
		gpusJSON += fmt.Sprintf(
			`{"index":%d,"name":%q,"vram_mb":%d,"numa_node":%d,"pcie_bus_id":%q}`,
			g.DeviceIndex, g.Name, g.VRAMMB, g.NUMANode, g.PCIeBusID,
		)
	}
	gpusJSON += "]"

	return fmt.Sprintf(
		`{"hostname":%q,"agent_version":%q,"cpu_model":%q,"cpu_cores":%d,`+
			`"ram_total_mb":%d,"numa_nodes":%d,"os":%q,"kernel":%q,"gpus":%s}`,
		snap.Hostname, snap.AgentVersion, snap.CPUModel, snap.CPUCores,
		snap.RAMTotalMB, snap.NUMANodes, snap.OSInfo, snap.KernelVer, gpusJSON,
	)
}
