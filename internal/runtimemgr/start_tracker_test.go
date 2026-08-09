package runtimemgr

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── B1: Basic TryStart / Done cycle ───────────────────────────────────────────

func TestB1_StartTracker_BasicCycle(t *testing.T) {
	tr := NewStartTracker()
	model := "llama3-8b"

	// Nothing running yet.
	if tr.IsStarting(model) {
		t.Fatal("expected IsStarting=false before any TryStart")
	}

	// First TryStart: should succeed.
	if !tr.TryStart(model) {
		t.Fatal("first TryStart should return true")
	}
	if !tr.IsStarting(model) {
		t.Fatal("IsStarting should be true after TryStart")
	}

	// Second TryStart while first is registered: must fail.
	if tr.TryStart(model) {
		t.Fatal("second TryStart while running should return false")
	}

	// Done releases the slot.
	tr.Done(model)
	if tr.IsStarting(model) {
		t.Fatal("IsStarting should be false after Done")
	}

	// TryStart works again after Done.
	if !tr.TryStart(model) {
		t.Fatal("TryStart should succeed after Done")
	}
	tr.Done(model)
}

// ── B2: Multiple models are independent ───────────────────────────────────────

func TestB2_StartTracker_IndependentModels(t *testing.T) {
	tr := NewStartTracker()
	a, b := "whisper-large", "llama3-70b"

	if !tr.TryStart(a) {
		t.Fatal("TryStart(a) should succeed")
	}
	// a is running; b is free.
	if tr.IsStarting(b) {
		t.Fatal("IsStarting(b) should be false while only a is running")
	}
	if !tr.TryStart(b) {
		t.Fatal("TryStart(b) should succeed independently of a")
	}

	// Both running.
	if !tr.IsStarting(a) || !tr.IsStarting(b) {
		t.Fatal("both models should be running")
	}

	tr.Done(a)
	if tr.IsStarting(a) {
		t.Fatal("a should no longer be running after Done")
	}
	if !tr.IsStarting(b) {
		t.Fatal("b should still be running")
	}
	tr.Done(b)
}

// ── B3: Exactly one goroutine launched for 100 concurrent requests ─────────────
//
// Simulates the proxy handler pattern: 100 concurrent requests all reach the
// cold-start branch simultaneously. Each calls IsStarting → TryStart and
// conditionally spawns a goroutine.  Invariant: exactly ONE goroutine is launched.
func TestB3_StartTracker_100Concurrent_OneGoroutine(t *testing.T) {
	tr := NewStartTracker()
	model := "vllm-70b"

	const requests = 100
	var launched int64 // counts how many goroutines were actually spawned

	var wg sync.WaitGroup
	wg.Add(requests)

	for i := 0; i < requests; i++ {
		go func() {
			defer wg.Done()
			// Mirror the exact proxy handler pattern.
			if !tr.IsStarting(model) {
				if tr.TryStart(model) {
					atomic.AddInt64(&launched, 1)
					// Simulate work then release.
					go func() {
						defer tr.Done(model)
						time.Sleep(5 * time.Millisecond)
					}()
				}
			}
			// All requests return 503 and don't block.
		}()
	}
	wg.Wait()

	if launched != 1 {
		t.Errorf("expected exactly 1 launched goroutine; got %d", launched)
	}
}

// ── B4: No goroutine explosion under retry storm ──────────────────────────────
//
// Simulates a retry storm: 100 requests arrive, one starts the goroutine.
// The goroutine takes 50ms. During that window 50 more retries arrive.
// Total goroutines launched must still be exactly 1.
func TestB4_StartTracker_RetryStorm_NoExplosion(t *testing.T) {
	tr := NewStartTracker()
	model := "tgi-mixtral"

	var launched int64

	launchIfNeeded := func() {
		if !tr.IsStarting(model) {
			if tr.TryStart(model) {
				atomic.AddInt64(&launched, 1)
				go func() {
					defer tr.Done(model)
					time.Sleep(50 * time.Millisecond)
				}()
			}
		}
	}

	// Wave 1: 100 concurrent requests.
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); launchIfNeeded() }()
	}
	wg.Wait()

	// Wave 2: 50 retries while goroutine is still running.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); launchIfNeeded() }()
	}
	wg.Wait()

	if launched != 1 {
		t.Errorf("retry storm: expected 1 goroutine; got %d", launched)
	}

	// Wait for goroutine to finish.
	deadline := time.Now().Add(200 * time.Millisecond)
	for tr.IsStarting(model) {
		if time.Now().After(deadline) {
			t.Fatal("goroutine did not finish in time")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// After goroutine finishes, a new cold start is allowed.
	if !tr.TryStart(model) {
		t.Fatal("TryStart should succeed after goroutine completed")
	}
	tr.Done(model)
}

// ── B5: TryStart/Done are safe when called from multiple goroutines ───────────

func TestB5_StartTracker_RaceSafety(t *testing.T) {
	tr := NewStartTracker()
	model := "embedder-large"

	// Run many rapid TryStart/Done cycles concurrently.
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tr.TryStart(model) {
				tr.Done(model)
			}
			_ = tr.IsStarting(model)
		}()
	}
	wg.Wait()
	// If we reach here without -race detector errors the test passes.
}

// ── B6: Second TryStart after Done — new start is permitted ──────────────────
//
// Covers the lazy-unload → cold-start scenario: the model was running,
// idle-evicted (Done called), and then a new request triggers a fresh cold start.
func TestB6_StartTracker_AfterUnload_FreshColdStart(t *testing.T) {
	tr := NewStartTracker()
	model := "whisper-small"

	// First deployment cycle.
	if !tr.TryStart(model) {
		t.Fatal("first deployment: TryStart must succeed")
	}
	tr.Done(model) // simulates model idle-eviction

	// After eviction, a new request triggers a fresh cold start.
	if !tr.TryStart(model) {
		t.Fatal("second deployment after unload: TryStart must succeed")
	}
	if !tr.IsStarting(model) {
		t.Fatal("model should be marked as starting after second TryStart")
	}
	tr.Done(model)

	if tr.IsStarting(model) {
		t.Fatal("model should be idle after second Done")
	}
}

// ── B7: Concurrent TryStart — exactly one winner even under race ─────────────
//
// All N goroutines call TryStart simultaneously. Exactly one must win.
func TestB7_StartTracker_ConcurrentTryStart_ExactlyOneWinner(t *testing.T) {
	for _, n := range []int{2, 10, 100, 500} {
		n := n
		t.Run("n="+itoa(n), func(t *testing.T) {
			t.Parallel()
			tr := NewStartTracker()
			model := "llama3-8b"
			var winners int64

			var wg sync.WaitGroup
			wg.Add(n)
			for i := 0; i < n; i++ {
				go func() {
					defer wg.Done()
					if tr.TryStart(model) {
						atomic.AddInt64(&winners, 1)
					}
				}()
			}
			wg.Wait()

			if winners != 1 {
				t.Errorf("n=%d: expected 1 winner; got %d", n, winners)
			}
			// Clean up so the test can be rerun.
			tr.Done(model)
		})
	}
}

func itoa(n int) string {
	buf := make([]byte, 0, 10)
	if n == 0 {
		return "0"
	}
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}
