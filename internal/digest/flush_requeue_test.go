package digest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
)

// failNTimesProvider is a minimal agent.Provider fake whose Run fails for the
// first failCount calls and then (if ever reached) returns result. Used to
// exercise flush's chunk-analysis failure path (Critical fix C4) without a
// live Claude CLI — analyzeWithRetry treats a plain error (not a "signal:
// killed"/context.DeadlineExceeded) as non-retryable, so each failNTimesProvider
// call maps 1:1 to one flush attempt's analyzeWithRetry call.
type failNTimesProvider struct {
	calls     atomic.Int64
	failCount int64
	result    *agent.RunResult
}

func (f *failNTimesProvider) Check() error { return nil }

func (f *failNTimesProvider) Run(_ context.Context, _ agent.RunOpts) (*agent.RunResult, error) {
	n := f.calls.Add(1)
	if n <= f.failCount {
		return nil, errors.New("simulated analysis failure")
	}
	if f.result != nil {
		return f.result, nil
	}
	return &agent.RunResult{Result: "[]"}, nil
}

func testDigestConfig() *config.DigestConfig {
	return &config.DigestConfig{
		MinConfidence:     0.5,
		AllowedCategories: []string{"bug"},
		MaxEstSize:        "small",
		MaxAutoSpawnHour:  10,
		MaxChunkSize:      50,
		ChunkTimeoutSecs:  30,
	}
}

// TestFlush_AnalysisFailure_RequeuesMessagesForNextFlush is the Critical fix
// (C4) happy path: when a chunk's analyzeWithRetry call errors, the prior
// code discarded the error and the chunk's messages vanished entirely. Now
// they must be requeued into e.buffer so the next flush retries them.
func TestFlush_AnalysisFailure_RequeuesMessagesForNextFlush(t *testing.T) {
	provider := &failNTimesProvider{failCount: 1}
	e := &Engine{
		cfg:   testDigestConfig(),
		agent: provider,
		model: "haiku",
	}
	e.buffer = []Message{
		{Channel: "C1", ChannelName: "errors", Text: "bug report one", Timestamp: "1"},
	}

	e.flush(context.Background())

	if provider.calls.Load() != 1 {
		t.Fatalf("expected exactly 1 analysis call on the failing flush, got %d", provider.calls.Load())
	}

	e.mu.Lock()
	buffered := len(e.buffer)
	e.mu.Unlock()
	if buffered != 1 {
		t.Fatalf("expected the failed chunk's 1 message to be requeued into the buffer, got %d buffered", buffered)
	}

	// The requeued message must still carry its original text (not a
	// dedup-annotated variant) so a subsequent dedup pass can still collapse
	// it against a genuinely new duplicate.
	e.mu.Lock()
	gotText := e.buffer[0].Text
	e.mu.Unlock()
	if gotText != "bug report one" {
		t.Errorf("expected requeued message text to be unchanged, got %q", gotText)
	}

	// Next flush: the provider now succeeds (failCount already exhausted at 1).
	e.flush(context.Background())
	if provider.calls.Load() != 2 {
		t.Fatalf("expected a 2nd analysis call on the retry flush, got %d", provider.calls.Load())
	}
	e.mu.Lock()
	buffered = len(e.buffer)
	e.mu.Unlock()
	if buffered != 0 {
		t.Errorf("expected the buffer to be empty after a successful retry flush, got %d buffered", buffered)
	}
}

// TestFlush_AnalysisFailsMaxTimes_DropsWithoutRequeue exercises the bound:
// after maxChunkRequeueAttempts consecutive analysis failures for the same
// message, it must be dropped (not requeued forever) — logged at Error.
func TestFlush_AnalysisFailsMaxTimes_DropsWithoutRequeue(t *testing.T) {
	provider := &failNTimesProvider{failCount: 1000} // always fails
	e := &Engine{
		cfg:   testDigestConfig(),
		agent: provider,
		model: "haiku",
	}
	e.buffer = []Message{
		{Channel: "C1", ChannelName: "errors", Text: "persistently broken message", Timestamp: "1"},
	}

	for i := 0; i < maxChunkRequeueAttempts; i++ {
		e.flush(context.Background())
	}

	if provider.calls.Load() != int64(maxChunkRequeueAttempts) {
		t.Fatalf("expected exactly %d analysis calls (one per attempt), got %d", maxChunkRequeueAttempts, provider.calls.Load())
	}

	e.mu.Lock()
	buffered := len(e.buffer)
	e.mu.Unlock()
	if buffered != 0 {
		t.Errorf("expected the message to be dropped (not requeued) after exhausting %d attempts, got %d still buffered", maxChunkRequeueAttempts, buffered)
	}

	// One more flush must be a no-op (nothing left to analyze).
	e.flush(context.Background())
	if provider.calls.Load() != int64(maxChunkRequeueAttempts) {
		t.Errorf("expected no further analysis calls once the message was dropped, got %d total calls", provider.calls.Load())
	}
}

// TestFlush_SuccessPath_Unchanged is a control: a chunk that analyzes
// cleanly (even to zero opportunities) must not be requeued or dropped —
// the buffer is simply empty afterward, same as before this fix.
func TestFlush_SuccessPath_Unchanged(t *testing.T) {
	provider := &failNTimesProvider{failCount: 0} // never fails
	e := &Engine{
		cfg:   testDigestConfig(),
		agent: provider,
		model: "haiku",
	}
	e.buffer = []Message{
		{Channel: "C1", ChannelName: "errors", Text: "just chatting, nothing actionable", Timestamp: "1"},
	}

	e.flush(context.Background())

	if provider.calls.Load() != 1 {
		t.Fatalf("expected exactly 1 analysis call, got %d", provider.calls.Load())
	}
	e.mu.Lock()
	buffered := len(e.buffer)
	e.mu.Unlock()
	if buffered != 0 {
		t.Errorf("expected an empty buffer after a successful (zero-opportunity) analysis, got %d buffered", buffered)
	}
	if e.totalProcessed.Load() != 1 {
		t.Errorf("expected totalProcessed=1, got %d", e.totalProcessed.Load())
	}
}
