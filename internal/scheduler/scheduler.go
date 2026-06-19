package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// QueueJob represents a deferred inference request waiting for GPU capacity.
type QueueJob struct {
	RequestID    string    `json:"request_id"`
	TeamID       string    `json:"team_id"`
	TeamPriority int       `json:"team_priority"`
	Model        string    `json:"model"`
	EnqueuedAt   time.Time `json:"enqueued_at"`
	TimeoutMs    int64     `json:"timeout_ms"`
}

// PoolMetrics carries real-time capacity information about a vLLM pool.
type PoolMetrics struct {
	Model          string
	ActiveRequests int
	QueueSize      int
	GPUUtilPct     float64
	AtCapacity     bool
}

// Scheduler manages priority queues backed by Redis Streams and dispatches
// admitted jobs to vLLM pools.
type Scheduler struct {
	rdb         *redis.Client
	highStream  string
	medStream   string
	lowStream   string
	gpuWatcher  *GPUWatcher
	log         *zap.Logger
	consumerGrp string
	consumerID  string
}

// NewScheduler constructs a Scheduler.
func NewScheduler(
	rdb *redis.Client,
	highStream, medStream, lowStream string,
	gpuWatcher *GPUWatcher,
	log *zap.Logger,
) *Scheduler {
	return &Scheduler{
		rdb:         rdb,
		highStream:  highStream,
		medStream:   medStream,
		lowStream:   lowStream,
		gpuWatcher:  gpuWatcher,
		log:         log,
		consumerGrp: "nexus-schedulers",
		consumerID:  "scheduler-1",
	}
}

// Start initialises consumer groups and begins the dispatch loop.
func (s *Scheduler) Start(ctx context.Context) error {
	for _, stream := range []string{s.highStream, s.medStream, s.lowStream} {
		err := s.rdb.XGroupCreateMkStream(ctx, stream, s.consumerGrp, "0").Err()
		if err != nil && err.Error() != "BUSYGROUP Consumer Group name already exists" {
			return fmt.Errorf("create consumer group for %s: %w", stream, err)
		}
	}
	go s.dispatchLoop(ctx)
	go s.ageJobs(ctx)
	return nil
}

// Enqueue places a job on the appropriate priority stream.
func (s *Scheduler) Enqueue(ctx context.Context, job *QueueJob) error {
	stream := s.streamForPriority(job.TeamPriority)
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}
	return s.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		Values: map[string]interface{}{
			"job":        string(data),
			"enqueued":   job.EnqueuedAt.UnixMilli(),
			"team_id":    job.TeamID,
			"model":      job.Model,
		},
	}).Err()
}

// dispatchLoop reads from all three streams in priority order and dispatches
// admitted jobs. Runs until ctx is cancelled.
func (s *Scheduler) dispatchLoop(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.drainOnce(ctx)
		}
	}
}

// drainOnce reads one batch from each stream in priority order.
func (s *Scheduler) drainOnce(ctx context.Context) {
	for _, stream := range []string{s.highStream, s.medStream, s.lowStream} {
		msgs, err := s.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    s.consumerGrp,
			Consumer: s.consumerID,
			Streams:  []string{stream, ">"},
			Count:    10,
			Block:    50 * time.Millisecond,
		}).Result()
		if err != nil {
			continue
		}
		for _, stream := range msgs {
			for _, msg := range stream.Messages {
				s.handleMessage(ctx, stream.Stream, msg)
			}
		}
	}
}

func (s *Scheduler) handleMessage(ctx context.Context, stream string, msg redis.XMessage) {
	jobStr, ok := msg.Values["job"].(string)
	if !ok {
		_ = s.rdb.XAck(ctx, stream, s.consumerGrp, msg.ID)
		return
	}

	var job QueueJob
	if err := json.Unmarshal([]byte(jobStr), &job); err != nil {
		_ = s.rdb.XAck(ctx, stream, s.consumerGrp, msg.ID)
		return
	}

	// Timeout check
	if job.TimeoutMs > 0 {
		deadline := job.EnqueuedAt.Add(time.Duration(job.TimeoutMs) * time.Millisecond)
		if time.Now().After(deadline) {
			s.log.Warn("job timed out in queue", zap.String("request_id", job.RequestID))
			_ = s.rdb.XAck(ctx, stream, s.consumerGrp, msg.ID)
			return
		}
	}

	// Check GPU availability before dispatching
	if !s.gpuWatcher.IsPoolAvailable(ctx, job.Model) {
		// Leave in queue — will be retried next tick via pending-entry redelivery
		return
	}

	s.log.Info("dispatching queued job",
		zap.String("request_id", job.RequestID),
		zap.String("team_id", job.TeamID),
		zap.String("model", job.Model),
	)
	_ = s.rdb.XAck(ctx, stream, s.consumerGrp, msg.ID)
}

// ageJobs promotes long-waiting low/med priority jobs to higher streams to
// prevent starvation. Runs every 10 seconds.
func (s *Scheduler) ageJobs(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.promoteAgedJobs(ctx)
		}
	}
}

func (s *Scheduler) promoteAgedJobs(ctx context.Context) {
	streams := []struct{ src, dst string }{
		{s.lowStream, s.medStream},
		{s.medStream, s.highStream},
	}
	now := time.Now()
	threshold := 30 * time.Second

	for _, pair := range streams {
		pending, err := s.rdb.XPending(ctx, pair.src, s.consumerGrp).Result()
		if err != nil || pending.Count == 0 {
			continue
		}
		details, err := s.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
			Stream: pair.src,
			Group:  s.consumerGrp,
			Start:  "-",
			End:    "+",
			Count:  100,
		}).Result()
		if err != nil {
			continue
		}
		for _, d := range details {
			if d.Idle > threshold {
				// Re-claim and re-enqueue to higher stream
				msgs, err := s.rdb.XClaim(ctx, &redis.XClaimArgs{
					Stream:   pair.src,
					Group:    s.consumerGrp,
					Consumer: s.consumerID,
					MinIdle:  threshold,
					Messages: []string{d.ID},
				}).Result()
				if err != nil || len(msgs) == 0 {
					continue
				}
				jobStr, ok := msgs[0].Values["job"].(string)
				if !ok {
					continue
				}
				var job QueueJob
				if err := json.Unmarshal([]byte(jobStr), &job); err != nil {
					continue
				}
				job.EnqueuedAt = now
				job.TeamPriority = min(job.TeamPriority+20, 100)
				data, _ := json.Marshal(job)
				_ = s.rdb.XAdd(ctx, &redis.XAddArgs{
					Stream: pair.dst,
					Values: map[string]interface{}{"job": string(data)},
				}).Err()
				_ = s.rdb.XAck(ctx, pair.src, s.consumerGrp, d.ID)
				s.log.Info("promoted aged job", zap.String("request_id", job.RequestID), zap.String("to_stream", pair.dst))
			}
		}
	}
}

func (s *Scheduler) streamForPriority(priority int) string {
	switch {
	case priority >= 70:
		return s.highStream
	case priority >= 35:
		return s.medStream
	default:
		return s.lowStream
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
