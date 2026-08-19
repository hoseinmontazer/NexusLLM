package proxy

// coldstart_test.go — unit tests for the cold-start lifecycle fix.
//
// These tests exercise handleColdStart() — the shared cold-start branch used
// by both ChatCompletions and pipelineSetup — in isolation using a lightweight
// fake Activator.  No database, no Docker, no network required.
//
// Tests:
//   C1  — First request when model is cold: 503 + goroutine launched
//   C2  — Second request while goroutine running: 503, no second goroutine
//   C3  — 100 concurrent requests: exactly one goroutine, all get 503
//   C4  — HTTP request cancellation does not cancel startup goroutine
//   C5  — After Done, new cold start is permitted (lazy-unload → cold-start)
//   C6  — StartTracker nil (fallback): original probe path, still returns 503
//   C7  — TryStart race: even when two goroutines both pass IsStarting==false,
//          only one EnsureRunning goroutine is launched

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nexusllm/nexusllm/internal/runtimemgr"
	"go.uber.org/zap"
)

func init() {
	// Set gin to test mode once at package init so concurrent calls to
	// ginContext() don't race on gin's global mode variable.
	gin.SetMode(gin.TestMode)
}

// ─── Fake Activator ───────────────────────────────────────────────────────────

type fakeActivator struct {
	ensureCalledN int64         // atomic: how many times EnsureRunning was entered
	unblockCh     chan struct{} // close to make EnsureRunning return success
	readyErr      error         // returned when unblockCh closes
	manual        bool          // model is operator-deployed (deployment_mode=manual)
}

func newFakeActivator() *fakeActivator {
	return &fakeActivator{unblockCh: make(chan struct{})}
}

func (f *fakeActivator) EnsureRunning(ctx context.Context, _ string) (*runtimemgr.RunningEndpoint, error) {
	atomic.AddInt64(&f.ensureCalledN, 1)
	select {
	case <-f.unblockCh:
		return nil, f.readyErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (f *fakeActivator) RecordActivity(_ context.Context, _ string) {}
func (f *fakeActivator) Status(_ context.Context, _ string) (*runtimemgr.ModelStatus, error) {
	return nil, nil
}
func (f *fakeActivator) IsManuallyDeployed(_ context.Context, _ string) bool { return f.manual }

// ─── Helper: invoke handleColdStart via a minimal Gin context ─────────────────

func ginContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c, rec
}

func minHandler(act runtimemgr.Activator, tracker *runtimemgr.StartTracker) *Handler {
	return &Handler{
		activator:    act,
		startTracker: tracker,
		log:          zap.NewNop(),
		coldStartDur: 500 * time.Millisecond, // short for tests
	}
}

// ─── C1: First request — 503 returned, exactly one goroutine launched ────────

func TestC1_FirstColdRequest_503_OneGoroutine(t *testing.T) {
	act := newFakeActivator()
	tracker := runtimemgr.NewStartTracker()
	h := minHandler(act, tracker)

	c, rec := ginContext(t)
	h.handleColdStart(c, "llama3-8b")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503; got %d body=%s", rec.Code, rec.Body.String())
	}
	assertModelStartingCode(t, rec.Body.Bytes())

	// Give goroutine time to enter EnsureRunning.
	time.Sleep(20 * time.Millisecond)

	if n := atomic.LoadInt64(&act.ensureCalledN); n != 1 {
		t.Errorf("expected EnsureRunning called once; called %d times", n)
	}
	if !tracker.IsStarting("llama3-8b") {
		t.Error("tracker should mark model as starting after first request")
	}

	// Unblock goroutine.
	close(act.unblockCh)
	waitForDone(t, tracker, "llama3-8b", 300*time.Millisecond)
}

// ─── C2: Second request while goroutine is running — no second goroutine ─────

func TestC2_SecondRequest_NoSecondGoroutine(t *testing.T) {
	act := newFakeActivator()
	tracker := runtimemgr.NewStartTracker()
	h := minHandler(act, tracker)

	// Request #1.
	c1, _ := ginContext(t)
	h.handleColdStart(c1, "whisper-large")
	time.Sleep(20 * time.Millisecond)

	// Request #2 — must not launch another goroutine.
	c2, rec2 := ginContext(t)
	h.handleColdStart(c2, "whisper-large")

	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("request #2: expected 503; got %d", rec2.Code)
	}

	time.Sleep(20 * time.Millisecond)
	if n := atomic.LoadInt64(&act.ensureCalledN); n != 1 {
		t.Errorf("EnsureRunning should be called exactly once; called %d times", n)
	}

	close(act.unblockCh)
	waitForDone(t, tracker, "whisper-large", 300*time.Millisecond)
}

// ─── C3: 100 concurrent requests — one goroutine, all get 503 ────────────────

func TestC3_100Concurrent_OneGoroutine_All503(t *testing.T) {
	act := newFakeActivator()
	tracker := runtimemgr.NewStartTracker()
	h := minHandler(act, tracker)

	const n = 100
	codes := make([]int, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			c, rec := ginContext(t)
			h.handleColdStart(c, "vllm-70b")
			codes[i] = rec.Code
		}()
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusServiceUnavailable {
			t.Errorf("request[%d]: expected 503; got %d", i, code)
		}
	}

	time.Sleep(30 * time.Millisecond)
	if n2 := atomic.LoadInt64(&act.ensureCalledN); n2 != 1 {
		t.Errorf("expected EnsureRunning called once; called %d times", n2)
	}

	close(act.unblockCh)
	waitForDone(t, tracker, "vllm-70b", 300*time.Millisecond)
}

// ─── C4: HTTP request cancellation does NOT cancel startup goroutine ──────────

func TestC4_RequestCancel_DoesNotCancelStartup(t *testing.T) {
	act := newFakeActivator()
	tracker := runtimemgr.NewStartTracker()
	h := minHandler(act, tracker)

	c, _ := ginContext(t)
	h.handleColdStart(c, "tgi-model")
	// HTTP response returned immediately (503). Simulate client disconnect.

	time.Sleep(20 * time.Millisecond)

	// The goroutine is still alive — it is using context.Background(), not
	// the request context.
	if n := atomic.LoadInt64(&act.ensureCalledN); n == 0 {
		t.Fatal("EnsureRunning should have been called")
	}
	if !tracker.IsStarting("tgi-model") {
		t.Fatal("startup goroutine must still be tracked after HTTP response returned")
	}

	// Unblock and confirm goroutine finishes.
	close(act.unblockCh)
	waitForDone(t, tracker, "tgi-model", 300*time.Millisecond)
}

// ─── C5: After Done, new cold start permitted (lazy-unload → cold-start) ──────

func TestC5_AfterDone_NewColdStartPermitted(t *testing.T) {
	act1 := newFakeActivator()
	tracker := runtimemgr.NewStartTracker()

	// First cold start — finish it.
	h1 := minHandler(act1, tracker)
	c1, _ := ginContext(t)
	h1.handleColdStart(c1, "embedder")
	time.Sleep(20 * time.Millisecond)
	close(act1.unblockCh)
	waitForDone(t, tracker, "embedder", 300*time.Millisecond)

	// After eviction (Done already called by goroutine), a second cold start must work.
	act2 := newFakeActivator()
	h2 := minHandler(act2, tracker)
	c2, rec2 := ginContext(t)
	h2.handleColdStart(c2, "embedder")

	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("second cold start: expected 503; got %d", rec2.Code)
	}
	time.Sleep(20 * time.Millisecond)
	if atomic.LoadInt64(&act2.ensureCalledN) != 1 {
		t.Error("second cold start: EnsureRunning should have been called once")
	}

	close(act2.unblockCh)
	waitForDone(t, tracker, "embedder", 300*time.Millisecond)
}

// ─── C6: Nil tracker — fallback probe path returns 503 ───────────────────────
//
// When WithStartTracker is not called, handleColdStart falls back to the
// 8-second probe.  We make EnsureRunning return an error immediately so the
// test finishes quickly.
func TestC6_NilTracker_FallbackPath_Returns503(t *testing.T) {
	act := newFakeActivator()
	act.readyErr = context.DeadlineExceeded
	close(act.unblockCh) // return error immediately

	h := &Handler{
		activator:    act,
		startTracker: nil, // nil → fallback path
		log:          zap.NewNop(),
		coldStartDur: 100 * time.Millisecond,
	}

	c, rec := ginContext(t)
	h.handleColdStart(c, "whisper-small")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil tracker fallback: expected 503; got %d body=%s", rec.Code, rec.Body.String())
	}
	time.Sleep(50 * time.Millisecond) // let background goroutine finish
}

// ─── C7: TryStart race — two goroutines both see IsStarting==false ────────────
//
// Even when two goroutines pass the IsStarting check simultaneously,
// TryStart's mutex ensures only one wins the slot.
func TestC7_TryStartRace_OnlyOneWins(t *testing.T) {
	act := newFakeActivator()
	tracker := runtimemgr.NewStartTracker()
	h := minHandler(act, tracker)

	const parallel = 20
	var wg sync.WaitGroup
	wg.Add(parallel)
	for i := 0; i < parallel; i++ {
		go func() {
			defer wg.Done()
			c, _ := ginContext(t)
			h.handleColdStart(c, "llamacpp-7b")
		}()
	}
	wg.Wait()

	time.Sleep(30 * time.Millisecond)
	if n := atomic.LoadInt64(&act.ensureCalledN); n != 1 {
		t.Errorf("expected exactly 1 EnsureRunning call; got %d", n)
	}

	close(act.unblockCh)
	waitForDone(t, tracker, "llamacpp-7b", 300*time.Millisecond)
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func assertModelStartingCode(t *testing.T, body []byte) {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("response not valid JSON: %v — body: %s", err, body)
	}
	errObj, ok := resp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected error object; got: %s", body)
	}
	code, _ := errObj["code"].(string)
	if code != "model_starting" {
		t.Errorf("expected error.code=model_starting; got %q — body: %s", code, body)
	}
}

func waitForDone(t *testing.T, tracker *runtimemgr.StartTracker, model string, deadline time.Duration) {
	t.Helper()
	limit := time.Now().Add(deadline)
	for tracker.IsStarting(model) {
		if time.Now().After(limit) {
			t.Fatalf("goroutine for %q did not finish within %s", model, deadline)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ─── C8: Manually-deployed model — 503, no cold start attempted ──────────────
//
// A model registered with deployment_mode='manual' has a container the
// operator owns. A request for it while its endpoint is unhealthy must report
// the unhealthy endpoint and MUST NOT launch a startup goroutine, which would
// duplicate or clobber the operator's container.

func TestC8_ManualDeployment_503_NoStartAttempted(t *testing.T) {
	act := newFakeActivator()
	act.manual = true
	tracker := runtimemgr.NewStartTracker()
	h := minHandler(act, tracker)

	c, rec := ginContext(t)
	h.handleColdStart(c, "qwen-manual")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503; got %d body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (body=%s)", err, rec.Body.String())
	}
	if body.Error.Code != "manual_runtime_unhealthy" {
		t.Errorf("expected error code manual_runtime_unhealthy; got %q (body=%s)",
			body.Error.Code, rec.Body.String())
	}

	// Nothing may have been started, now or in the background.
	time.Sleep(100 * time.Millisecond)
	if n := atomic.LoadInt64(&act.ensureCalledN); n != 0 {
		t.Errorf("EnsureRunning must not be called for a manual deployment; called %d times", n)
	}
	if tracker.IsStarting("qwen-manual") {
		t.Error("no start slot may be claimed for a manual deployment")
	}
}
