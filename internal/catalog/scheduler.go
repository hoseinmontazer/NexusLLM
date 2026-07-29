package catalog

import (
	"context"
	"sync"
	"time"

	"github.com/nexusllm/nexusllm/internal/runtime"
	"go.uber.org/zap"

	"github.com/jmoiron/sqlx"
)

// SyncScheduler manages per-provider background sync timers.
// It starts one goroutine per provider that has catalog_sync_enabled=TRUE,
// and supports on-demand manual sync via TriggerSync.
type SyncScheduler struct {
	syncer  *CatalogSyncer
	store   *ProviderStore
	log     *zap.Logger

	mu       sync.Mutex
	triggers map[string]chan struct{} // provider_id → trigger channel
}

// NewSyncScheduler constructs a SyncScheduler.
func NewSyncScheduler(db *sqlx.DB, factory *runtime.Factory, log *zap.Logger) *SyncScheduler {
	return &SyncScheduler{
		syncer:   NewCatalogSyncer(db, factory, log),
		store:    NewProviderStore(db),
		log:      log,
		triggers: make(map[string]chan struct{}),
	}
}

// Start launches background sync goroutines for all enabled providers.
// Blocks until ctx is cancelled. Safe to call in a goroutine.
func (s *SyncScheduler) Start(ctx context.Context) {
	providers, err := s.store.ListSyncEnabled(ctx)
	if err != nil {
		s.log.Warn("catalog scheduler: failed to load providers", zap.Error(err))
		return
	}

	var wg sync.WaitGroup
	for _, p := range providers {
		p := p // capture
		trig := make(chan struct{}, 1)
		s.mu.Lock()
		s.triggers[p.ID] = trig
		s.mu.Unlock()

		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runLoop(ctx, &p, trig)
		}()
	}

	// Watch for context cancellation.
	<-ctx.Done()
	wg.Wait()
}

// TriggerSync sends an immediate sync signal to the provider's goroutine.
// If the provider is not currently running a scheduled sync loop
// (e.g. catalog_sync_enabled=FALSE), it runs a one-shot sync directly.
func (s *SyncScheduler) TriggerSync(ctx context.Context, providerID string) error {
	s.mu.Lock()
	trig, ok := s.triggers[providerID]
	s.mu.Unlock()

	if ok {
		// Signal the background loop (non-blocking).
		select {
		case trig <- struct{}{}:
		default:
			// Already queued — that's fine.
		}
		return nil
	}

	// No running loop — do a direct one-shot sync.
	return s.syncer.SyncProvider(ctx, providerID)
}

// runLoop is the per-provider background goroutine.
func (s *SyncScheduler) runLoop(ctx context.Context, p *Provider, trig <-chan struct{}) {
	interval := s.syncer.SyncInterval(p)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Sync immediately on startup.
	s.doSync(ctx, p.ID)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.doSync(ctx, p.ID)
		case <-trig:
			s.doSync(ctx, p.ID)
		}
	}
}

func (s *SyncScheduler) doSync(ctx context.Context, providerID string) {
	syncCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := s.syncer.SyncProvider(syncCtx, providerID); err != nil {
		s.log.Warn("catalog sync error", zap.String("provider_id", providerID), zap.Error(err))
	}
}
