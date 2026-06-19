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

	// Default per-model capacity (configurable via env in production)
	capacity := map[string]int{
		"gemma-27b":     50,
		"llama-3.3-70b": 30,
		"qwen-2.5-72b":  30,
	}

	gpuWatcher := scheduler.NewGPUWatcher(
		rdb,
		cfg.VLLM.Endpoints,
		capacity,
		cfg.VLLM.PollInterval,
		log,
	)

	sched := scheduler.NewScheduler(
		rdb,
		cfg.Scheduler.QueueHighStream,
		cfg.Scheduler.QueueMedStream,
		cfg.Scheduler.QueueLowStream,
		gpuWatcher,
		log,
	)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Start GPU watcher in background
	go gpuWatcher.Start(runCtx)

	// Start scheduler dispatch loop
	if err := sched.Start(runCtx); err != nil {
		log.Fatal("failed to start scheduler", zap.Error(err))
	}

	log.Info("nexus-scheduler running",
		zap.String("high_stream", cfg.Scheduler.QueueHighStream),
		zap.String("med_stream",  cfg.Scheduler.QueueMedStream),
		zap.String("low_stream",  cfg.Scheduler.QueueLowStream),
	)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("shutting down nexus-scheduler...")
	cancel()
	time.Sleep(500 * time.Millisecond) // drain in-flight
	log.Info("nexus-scheduler stopped")
}
