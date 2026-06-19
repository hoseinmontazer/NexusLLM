package config

import (
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config aggregates all subsystem configurations.
type Config struct {
	Server    ServerConfig
	Database  DatabaseConfig
	Redis     RedisConfig
	Auth      AuthConfig
	Scheduler SchedulerConfig
	VLLM      VLLMConfig
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
	JWTSecret        string
	APIKeyCacheTTL   time.Duration
	JWTCacheTTL      time.Duration
}

// SchedulerConfig names the Redis Streams used by the scheduler.
type SchedulerConfig struct {
	QueueHighStream string
	QueueMedStream  string
	QueueLowStream  string
	DispatchInterval time.Duration
}

// VLLMConfig holds model → endpoint mappings and poll interval.
type VLLMConfig struct {
	PollInterval time.Duration
	// Endpoints is a map of model name → base URL (e.g. "gemma-27b" → "http://vllm-gemma:8000")
	Endpoints map[string]string
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
	v.SetDefault("server.writetimeout", "120s")
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
	if len(cfg.VLLM.Endpoints) == 0 {
		// sensible local defaults matching docker-compose service names
		cfg.VLLM.Endpoints = map[string]string{
			"gemma-27b":     "http://vllm-gemma:8000",
			"llama-3.3-70b": "http://vllm-llama:8000",
			"qwen-2.5-72b":  "http://vllm-qwen:8000",
		}
	}

	return cfg, nil
}
