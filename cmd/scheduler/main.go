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

	// Per-model capacity — read from env NEXUS_VLLM_CAPACITY_<MODEL>=N
	// or left empty (defaults to 100 per model in GPUWatcher.maxCapacity).
	capacity := map[string]int{}

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
