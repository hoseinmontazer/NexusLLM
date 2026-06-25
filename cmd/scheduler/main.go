// cmd/scheduler — standalone GPU watcher process.
//
// This binary runs the GPUWatcher, which polls vLLM /metrics endpoints and
// publishes pool capacity data to Redis. The main scheduling engine
// (Scheduler) runs inside the admin process alongside the control plane.
//
// Run this if you want to decouple vLLM metric polling from the admin server,
// e.g. in a separate container or for monitoring purposes only.
//
// Usage:
//
//	NEXUS_REDIS_ADDR=redis:6379 ./nexus-scheduler
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nexusllm/nexusllm/internal/config"
	"github.com/nexusllm/nexusllm/internal/scheduler"
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
	log.Info("redis connected")

	// Per-model capacity: env NEXUS_VLLM_CAPACITY_<MODEL>=N, or default 100.
	capacity := map[string]int{}

	gpuWatcher := scheduler.NewGPUWatcher(
		rdb,
		cfg.VLLM.Endpoints,
		capacity,
		cfg.VLLM.PollInterval,
		log,
	)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	log.Info("nexus-scheduler (gpu-watcher) starting",
		zap.Int("endpoints", len(cfg.VLLM.Endpoints)),
		zap.Duration("poll_interval", cfg.VLLM.PollInterval),
	)

	// Run GPU watcher — blocks until signal
	go gpuWatcher.Start(runCtx)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("shutting down nexus-scheduler...")
	cancel()
	time.Sleep(200 * time.Millisecond)
	log.Info("nexus-scheduler stopped")
}
