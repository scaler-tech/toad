package agent

import (
	"context"
	"errors"
	"testing"
)

// fakeProvider is a minimal Provider test double whose Run behavior is
// driven by a queue of canned (result, error) pairs, one per call — used to
// exercise FailureTrackingProvider's success/failure sequencing without a
// live Claude CLI.
type fakeProvider struct {
	results []*RunResult
	errs    []error
	i       int
}

func (f *fakeProvider) Check() error { return nil }

func (f *fakeProvider) Run(context.Context, RunOpts) (*RunResult, error) {
	idx := f.i
	f.i++
	var res *RunResult
	var err error
	if idx < len(f.results) {
		res = f.results[idx]
	}
	if idx < len(f.errs) {
		err = f.errs[idx]
	}
	return res, err
}

// TestFailureTrackingProvider_IncrementsOnFailure verifies consecutive
// failures accumulate and the last error text is captured (Critical fix C5).
func TestFailureTrackingProvider_IncrementsOnFailure(t *testing.T) {
	fp := &fakeProvider{errs: []error{
		errors.New("first failure"),
		errors.New("second failure"),
		errors.New("third failure"),
	}}
	p := &FailureTrackingProvider{Provider: fp}

	for i := 0; i < 3; i++ {
		if _, err := p.Run(context.Background(), RunOpts{}); err == nil {
			t.Fatalf("call %d: expected an error", i)
		}
	}

	snap := p.Snapshot()
	if snap.Consecutive != 3 {
		t.Errorf("expected Consecutive=3, got %d", snap.Consecutive)
	}
	if snap.LastErr != "third failure" {
		t.Errorf("expected LastErr=%q, got %q", "third failure", snap.LastErr)
	}
	if !snap.LastSuccessAt.IsZero() {
		t.Errorf("expected LastSuccessAt to remain zero with no successes yet, got %v", snap.LastSuccessAt)
	}
}

// TestFailureTrackingProvider_ResetsOnSuccess verifies a successful Run call
// resets the consecutive-failure counter and records the success time, even
// after a run of prior failures.
func TestFailureTrackingProvider_ResetsOnSuccess(t *testing.T) {
	fp := &fakeProvider{
		results: []*RunResult{nil, nil, {Result: "ok"}},
		errs:    []error{errors.New("failure 1"), errors.New("failure 2"), nil},
	}
	p := &FailureTrackingProvider{Provider: fp}

	for i := 0; i < 2; i++ {
		if _, err := p.Run(context.Background(), RunOpts{}); err == nil {
			t.Fatalf("call %d: expected an error", i)
		}
	}
	if p.Snapshot().Consecutive != 2 {
		t.Fatalf("expected Consecutive=2 before the success, got %d", p.Snapshot().Consecutive)
	}

	result, err := p.Run(context.Background(), RunOpts{})
	if err != nil {
		t.Fatalf("unexpected error on the successful call: %v", err)
	}
	if result.Result != "ok" {
		t.Errorf("expected the wrapped result to pass through unchanged, got %+v", result)
	}

	snap := p.Snapshot()
	if snap.Consecutive != 0 {
		t.Errorf("expected Consecutive to reset to 0 after a success, got %d", snap.Consecutive)
	}
	if snap.LastSuccessAt.IsZero() {
		t.Error("expected LastSuccessAt to be set after a success")
	}
	// LastErr is a historical record of the most recent failure — it is not
	// cleared by a subsequent success (still useful context for "when did
	// this last fail"), only Consecutive resets.
	if snap.LastErr != "failure 2" {
		t.Errorf("expected LastErr to still reflect the last failure, got %q", snap.LastErr)
	}
}

// TestFailureTrackingProvider_TruncatesLongErrors keeps daemon_stats from
// bloating on a pathologically long error message (e.g. a huge stderr dump).
func TestFailureTrackingProvider_TruncatesLongErrors(t *testing.T) {
	longMsg := ""
	for i := 0; i < 500; i++ {
		longMsg += "x"
	}
	fp := &fakeProvider{errs: []error{errors.New(longMsg)}}
	p := &FailureTrackingProvider{Provider: fp}

	if _, err := p.Run(context.Background(), RunOpts{}); err == nil {
		t.Fatal("expected an error")
	}

	snap := p.Snapshot()
	if len(snap.LastErr) != maxTrackedErrLen {
		t.Errorf("expected LastErr truncated to %d chars, got %d", maxTrackedErrLen, len(snap.LastErr))
	}
}

// TestFailureTrackingProvider_SnapshotZeroValue verifies a freshly
// constructed provider (no calls yet) reports a zero-value snapshot rather
// than anything misleading.
func TestFailureTrackingProvider_SnapshotZeroValue(t *testing.T) {
	p := &FailureTrackingProvider{Provider: &fakeProvider{}}
	snap := p.Snapshot()
	if snap.Consecutive != 0 || snap.LastErr != "" || !snap.LastSuccessAt.IsZero() {
		t.Errorf("expected a zero-value snapshot before any Run calls, got %+v", snap)
	}
}
