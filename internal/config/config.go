package config

import (
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config aggregates all subsystem configurations.
type Config struct {
	Server     ServerConfig
	Database   DatabaseConfig
	Redis      RedisConfig
	Auth       AuthConfig
	Scheduler  SchedulerConfig
	VLLM       VLLMConfig
	RuntimeMgr RuntimeMgrConfig
}

// ServerConfig controls the HTTP listener.
type ServerConfig struct {
	Port            string
	MetricsPort     string
	Mode            string // debug | release
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
}

// DatabaseConfig controls the PostgreSQL connection pool.
type DatabaseConfig struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// RedisConfig controls the Redis client.
type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

// AuthConfig controls JWT and API-key validation.
type AuthConfig struct {
	JWTSecret      string
	APIKeyCacheTTL time.Duration
	JWTCacheTTL    time.Duration
}

// SchedulerConfig names the Redis Streams used by the scheduler.
type SchedulerConfig struct {
	QueueHighStream  string
	QueueMedStream   string
	QueueLowStream   string
	DispatchInterval time.Duration
}

// VLLMConfig holds model → endpoint mappings and poll interval.
type VLLMConfig struct {
	PollInterval time.Duration
	// Endpoints is a map of model name → base URL (e.g. "gemma-27b" → "http://vllm-gemma:8000")
	Endpoints map[string]string
}

// RuntimeMgrConfig controls the lazy-load runtime manager.
type RuntimeMgrConfig struct {
	// DefaultIdleTimeout is how long a container stays running with no traffic.
	// Override per-model via model_runtime_configs.idle_timeout_secs.
	// Env: NEXUS_RUNTIMEMGR_IDLETIMEOUT  (default: 15m)
	DefaultIdleTimeout time.Duration

	// ColdStartTimeout is the max time EnsureRunning waits for a model to become healthy.
	// Env: NEXUS_RUNTIMEMGR_COLDSTARTTIMEOUT  (default: 5m)
	ColdStartTimeout time.Duration

	// DefaultModelsVolume is the Docker volume or host path mounted as /models.
	// Env: NEXUS_RUNTIMEMGR_MODELSVOLUME  (default: llamacpp_models)
	DefaultModelsVolume string

	// DefaultImage is the default llama-server Docker image.
	// Env: NEXUS_RUNTIMEMGR_DEFAULTIMAGE  (default: ghcr.io/ggml-org/llama.cpp:server)
	DefaultImage string
}

// Load reads configuration from environment variables (prefix NEXUS_) and any
// config file found on the search path, then applies sensible defaults.
func Load() (*Config, error) {
	v := viper.New()

	// --- defaults ---
	v.SetDefault("server.port", "8080")
	v.SetDefault("server.metricsport", "9090")
	v.SetDefault("server.mode", "release")
	v.SetDefault("server.readtimeout", "30s")
	v.SetDefault("server.writetimeout", "30m") // must exceed runtimemgr.coldstarttimeout
	v.SetDefault("server.shutdowntimeout", "15s")

	v.SetDefault("database.dsn", "postgres://nexus:nexus@localhost:5432/nexusllm?sslmode=disable")
	v.SetDefault("database.maxopenconns", 20)
	v.SetDefault("database.maxidleconns", 5)
	v.SetDefault("database.connmaxlifetime", "5m")

	v.SetDefault("redis.addr", "localhost:6379")
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)

	v.SetDefault("auth.jwtsecret", "change-me-in-production")
	v.SetDefault("auth.apikeycachettl", "5m")
	v.SetDefault("auth.jwtcachettl", "1m")

	v.SetDefault("scheduler.queuehighstream", "nexus:queue:high")
	v.SetDefault("scheduler.queuemedstream", "nexus:queue:med")
	v.SetDefault("scheduler.queuelowstream", "nexus:queue:low")
	v.SetDefault("scheduler.dispatchinterval", "100ms")

	v.SetDefault("vllm.pollinterval", "5s")

	v.SetDefault("runtimemgr.idletimeout", "15m")
	v.SetDefault("runtimemgr.coldstarttimeout", "20m")
	v.SetDefault("runtimemgr.modelsvolume", "llamacpp_models")
	v.SetDefault("runtimemgr.defaultimage", "ghcr.io/ggml-org/llama.cpp:server")

	// --- env ---
	v.SetEnvPrefix("NEXUS")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// --- optional config file ---
	v.SetConfigName("nexus")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/nexus")
	_ = v.ReadInConfig() // not fatal if missing

	cfg := &Config{}

	// Server
	cfg.Server.Port = v.GetString("server.port")
	cfg.Server.MetricsPort = v.GetString("server.metricsport")
	cfg.Server.Mode = v.GetString("server.mode")
	cfg.Server.ReadTimeout = v.GetDuration("server.readtimeout")
	cfg.Server.WriteTimeout = v.GetDuration("server.writetimeout")
	cfg.Server.ShutdownTimeout = v.GetDuration("server.shutdowntimeout")

	// Database
	cfg.Database.DSN = v.GetString("database.dsn")
	cfg.Database.MaxOpenConns = v.GetInt("database.maxopenconns")
	cfg.Database.MaxIdleConns = v.GetInt("database.maxidleconns")
	cfg.Database.ConnMaxLifetime = v.GetDuration("database.connmaxlifetime")

	// Redis
	cfg.Redis.Addr = v.GetString("redis.addr")
	cfg.Redis.Password = v.GetString("redis.password")
	cfg.Redis.DB = v.GetInt("redis.db")

	// Auth
	cfg.Auth.JWTSecret = v.GetString("auth.jwtsecret")
	cfg.Auth.APIKeyCacheTTL = v.GetDuration("auth.apikeycachettl")
	cfg.Auth.JWTCacheTTL = v.GetDuration("auth.jwtcachettl")

	// Scheduler
	cfg.Scheduler.QueueHighStream = v.GetString("scheduler.queuehighstream")
	cfg.Scheduler.QueueMedStream = v.GetString("scheduler.queuemedstream")
	cfg.Scheduler.QueueLowStream = v.GetString("scheduler.queuelowstream")
	cfg.Scheduler.DispatchInterval = v.GetDuration("scheduler.dispatchinterval")

	// VLLM
	cfg.VLLM.PollInterval = v.GetDuration("vllm.pollinterval")
	cfg.VLLM.Endpoints = v.GetStringMapString("vllm.endpoints")
	// No hardcoded defaults — endpoints are populated dynamically from the
	// model registry (model_endpoints table). Configure via env if needed:
	//   NEXUS_VLLM_ENDPOINTS_<MODEL>=http://host:port

	// RuntimeMgr
	cfg.RuntimeMgr.DefaultIdleTimeout = v.GetDuration("runtimemgr.idletimeout")
	cfg.RuntimeMgr.ColdStartTimeout = v.GetDuration("runtimemgr.coldstarttimeout")
	cfg.RuntimeMgr.DefaultModelsVolume = v.GetString("runtimemgr.modelsvolume")
	cfg.RuntimeMgr.DefaultImage = v.GetString("runtimemgr.defaultimage")

	return cfg, nil
}
