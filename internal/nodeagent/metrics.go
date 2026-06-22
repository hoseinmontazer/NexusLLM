package nodeagent

// metrics.go — Prometheus metrics exported by the node agent.
// The agent exposes these on :9092/metrics (separate from gateway :9090 and admin :9091).

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// AgentMetrics holds all Prometheus gauges and counters for the node agent.
type AgentMetrics struct {
	// Agent liveness
	AgentUp *prometheus.GaugeVec // agent_up{node_id, hostname}

	// Task execution
	TasksTotal    *prometheus.CounterVec // agent_tasks_total{node_id, task_type, status}
	TaskDuration  *prometheus.HistogramVec // agent_task_duration_seconds{node_id, task_type}

	// Runtime tracking
	RuntimeCount  *prometheus.GaugeVec // agent_runtime_count{node_id, backend, state}

	// Hardware metrics (updated from telemetry collection)
	CPUUsage     *prometheus.GaugeVec // agent_cpu_usage{node_id} — utilization %
	MemoryUsage  *prometheus.GaugeVec // agent_memory_usage{node_id} — used MB
	MemoryTotal  *prometheus.GaugeVec // agent_memory_total{node_id} — total MB

	// GPU metrics (per device)
	GPUMemoryUsed  *prometheus.GaugeVec // agent_gpu_memory_used{node_id, device_index}
	GPUMemoryTotal *prometheus.GaugeVec // agent_gpu_memory_total{node_id, device_index}
	GPUUtilization *prometheus.GaugeVec // agent_gpu_utilization{node_id, device_index}
	GPUTemperature *prometheus.GaugeVec // agent_gpu_temperature_celsius{node_id, device_index}
	GPUPowerDraw   *prometheus.GaugeVec // agent_gpu_power_draw_watts{node_id, device_index}
}

// NewAgentMetrics registers and returns all agent Prometheus metrics.
func NewAgentMetrics(nodeID, hostname string) *AgentMetrics {
	constLabels := prometheus.Labels{"node_id": nodeID, "hostname": hostname}

	m := &AgentMetrics{
		AgentUp: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   "nexus",
			Subsystem:   "agent",
			Name:        "up",
			Help:        "1 if the node agent is running and connected to the control plane.",
			ConstLabels: constLabels,
		}, []string{}),

		TasksTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace:   "nexus",
			Subsystem:   "agent",
			Name:        "tasks_total",
			Help:        "Total number of tasks executed by this agent.",
			ConstLabels: constLabels,
		}, []string{"task_type", "status"}),

		TaskDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace:   "nexus",
			Subsystem:   "agent",
			Name:        "task_duration_seconds",
			Help:        "Duration of task execution in seconds.",
			ConstLabels: constLabels,
			Buckets:     []float64{0.1, 0.5, 1, 5, 10, 30, 60, 120, 300},
		}, []string{"task_type"}),

		RuntimeCount: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   "nexus",
			Subsystem:   "agent",
			Name:        "runtime_count",
			Help:        "Number of runtimes in each state on this node.",
			ConstLabels: constLabels,
		}, []string{"backend", "state"}),

		CPUUsage: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   "nexus",
			Subsystem:   "agent",
			Name:        "cpu_usage",
			Help:        "CPU utilization percentage on this node.",
			ConstLabels: constLabels,
		}, []string{}),

		MemoryUsage: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   "nexus",
			Subsystem:   "agent",
			Name:        "memory_usage",
			Help:        "RAM used in megabytes on this node.",
			ConstLabels: constLabels,
		}, []string{}),

		MemoryTotal: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   "nexus",
			Subsystem:   "agent",
			Name:        "memory_total",
			Help:        "Total RAM in megabytes on this node.",
			ConstLabels: constLabels,
		}, []string{}),

		GPUMemoryUsed: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   "nexus",
			Subsystem:   "agent",
			Name:        "gpu_memory_used",
			Help:        "GPU VRAM used in megabytes.",
			ConstLabels: constLabels,
		}, []string{"device_index", "device_name"}),

		GPUMemoryTotal: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   "nexus",
			Subsystem:   "agent",
			Name:        "gpu_memory_total",
			Help:        "Total GPU VRAM in megabytes.",
			ConstLabels: constLabels,
		}, []string{"device_index", "device_name"}),

		GPUUtilization: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   "nexus",
			Subsystem:   "agent",
			Name:        "gpu_utilization",
			Help:        "GPU compute utilization percentage.",
			ConstLabels: constLabels,
		}, []string{"device_index", "device_name"}),

		GPUTemperature: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   "nexus",
			Subsystem:   "agent",
			Name:        "gpu_temperature_celsius",
			Help:        "GPU temperature in Celsius.",
			ConstLabels: constLabels,
		}, []string{"device_index", "device_name"}),

		GPUPowerDraw: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   "nexus",
			Subsystem:   "agent",
			Name:        "gpu_power_draw_watts",
			Help:        "GPU power draw in Watts.",
			ConstLabels: constLabels,
		}, []string{"device_index", "device_name"}),
	}

	// Mark agent as up immediately
	m.AgentUp.With(prometheus.Labels{}).Set(1)
	return m
}

// StartMetricsServer starts a Prometheus metrics HTTP server on the given address.
// Call this in a goroutine.
func StartMetricsServer(addr string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	_ = srv.ListenAndServe()
}
