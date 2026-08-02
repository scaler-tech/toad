package cmd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/state"
)

// --- incrementMetric ---

func TestIncrementMetric_NilDBIsNoop(t *testing.T) {
	// Must not panic — handlers call this with deps.stateManager.DB(), which
	// is nil for the in-memory state.NewManager() used by tests/CLI one-shots.
	incrementMetric(nil, "intake")
}

func TestIncrementMetric_WritesToDB(t *testing.T) {
	db := newTestDB(t)
	incrementMetric(db, "intake")
	incrementMetric(db, "intake")
	incrementMetric(db, "qa")

	now := time.Now()
	intake := db.MetricSeries("intake", 1, now)
	if len(intake) != 1 || intake[0] != 2 {
		t.Errorf("intake series = %v, want [2]", intake)
	}
	qa := db.MetricSeries("qa", 1, now)
	if len(qa) != 1 || qa[0] != 1 {
		t.Errorf("qa series = %v, want [1]", qa)
	}
}

// --- concurrencyGauge ---

func TestConcurrencyGauge_ReportsSlotsAndInFlight(t *testing.T) {
	sem := make(chan struct{}, 3)

	slots, inFlight := concurrencyGauge(sem)
	if slots != 3 || inFlight != 0 {
		t.Fatalf("empty semaphore: got slots=%d inFlight=%d, want 3,0", slots, inFlight)
	}

	sem <- struct{}{}
	sem <- struct{}{}
	slots, inFlight = concurrencyGauge(sem)
	if slots != 3 || inFlight != 2 {
		t.Fatalf("2 held: got slots=%d inFlight=%d, want 3,2", slots, inFlight)
	}

	<-sem
	slots, inFlight = concurrencyGauge(sem)
	if slots != 3 || inFlight != 1 {
		t.Fatalf("1 held after release: got slots=%d inFlight=%d, want 3,1", slots, inFlight)
	}
}

// TestConcurrencyGauge_ConcurrentAcquireRelease exercises concurrencyGauge
// under actual concurrent acquire/release traffic (the real usage pattern:
// N goroutines racing to hold a bounded number of slots) so `go test -race`
// can confirm reading len/cap on a channel while other goroutines send/
// receive on it is race-free.
func TestConcurrencyGauge_ConcurrentAcquireRelease(t *testing.T) {
	const slots = 4
	const workers = 20
	sem := make(chan struct{}, slots)

	var workersWG sync.WaitGroup
	stop := make(chan struct{})
	readerDone := make(chan struct{})

	// A reader goroutine continuously snapshots the gauge while workers
	// acquire/release — this is what root.go's 10s stats ticker does
	// concurrently with handlers.go/ticketflow.go's semaphore traffic. Kept
	// off workersWG deliberately: it only stops once `stop` is closed
	// (after the workers finish), so waiting on it in the same group would
	// deadlock.
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
				gotSlots, inFlight := concurrencyGauge(sem)
				if gotSlots != slots {
					t.Errorf("slots changed under load: got %d, want %d", gotSlots, slots)
				}
				if inFlight < 0 || inFlight > slots {
					t.Errorf("inFlight out of range: got %d, want 0..%d", inFlight, slots)
				}
			}
		}
	}()

	for i := 0; i < workers; i++ {
		workersWG.Add(1)
		go func() {
			defer workersWG.Done()
			for j := 0; j < 25; j++ {
				sem <- struct{}{}
				<-sem
			}
		}()
	}

	// Let workers run to completion, then stop the reader and wait for it too.
	workersDone := make(chan struct{})
	go func() {
		workersWG.Wait()
		close(workersDone)
	}()
	select {
	case <-workersDone:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent acquire/release did not finish within 2s")
	}
	close(stop)
	<-readerDone
}

// --- repoSyncTracker ---

func TestRepoSyncTracker_SnapshotEmptyIsNil(t *testing.T) {
	tr := newRepoSyncTracker()
	if snap := tr.snapshot(); snap != nil {
		t.Errorf("expected nil snapshot before any record, got %v", snap)
	}
}

func TestRepoSyncTracker_RecordSuccessThenFailure(t *testing.T) {
	tr := newRepoSyncTracker()

	tr.record("app", nil)
	snap := tr.snapshot()
	st, ok := snap["app"]
	if !ok {
		t.Fatal("expected 'app' entry after a successful record")
	}
	if st.LastSyncAt.IsZero() {
		t.Error("expected LastSyncAt to be set on success")
	}
	if st.LastError != "" {
		t.Errorf("expected no error after success, got %q", st.LastError)
	}
	firstSync := st.LastSyncAt

	// A subsequent failure sets LastError but must not clobber the
	// last-known-good LastSyncAt (the dashboard wants "last successful sync",
	// not "last attempt").
	tr.record("app", errors.New("fetch failed: network unreachable"))
	snap = tr.snapshot()
	st = snap["app"]
	if st.LastError != "fetch failed: network unreachable" {
		t.Errorf("LastError = %q, want the failure message", st.LastError)
	}
	if !st.LastSyncAt.Equal(firstSync) {
		t.Errorf("LastSyncAt changed on failure: got %v, want unchanged %v", st.LastSyncAt, firstSync)
	}

	// A later success clears LastError again.
	tr.record("app", nil)
	snap = tr.snapshot()
	st = snap["app"]
	if st.LastError != "" {
		t.Errorf("expected LastError cleared after a subsequent success, got %q", st.LastError)
	}
}

func TestRepoSyncTracker_SnapshotIsIndependentCopy(t *testing.T) {
	tr := newRepoSyncTracker()
	tr.record("app", nil)
	snap := tr.snapshot()
	snap["app"] = state.RepoSyncStatus{LastError: "mutated by caller"}

	fresh := tr.snapshot()
	if fresh["app"].LastError != "" {
		t.Error("mutating a returned snapshot leaked back into the tracker's internal state")
	}
}

func TestRepoSyncTracker_ConcurrentRecordAndSnapshot(t *testing.T) {
	tr := newRepoSyncTracker()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				err = errors.New("boom")
			}
			for j := 0; j < 20; j++ {
				tr.record("repo", err)
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = tr.snapshot()
			}
		}()
	}
	wg.Wait()
}

func TestRepoSyncTracker_WrapRecordsOutcomeAndPropagatesResult(t *testing.T) {
	tr := newRepoSyncTracker()
	boom := errors.New("boom")
	calls := 0
	inner := func(_ context.Context, repo config.RepoConfig) error {
		calls++
		if repo.Name == "bad-repo" {
			return boom
		}
		return nil
	}
	wrapped := tr.wrap(inner)

	if err := wrapped(context.Background(), config.RepoConfig{Name: "good-repo"}); err != nil {
		t.Fatalf("wrapped(good-repo): %v", err)
	}
	if err := wrapped(context.Background(), config.RepoConfig{Name: "bad-repo"}); !errors.Is(err, boom) {
		t.Fatalf("wrapped(bad-repo) = %v, want %v", err, boom)
	}
	if calls != 2 {
		t.Fatalf("inner syncer called %d times, want 2", calls)
	}

	snap := tr.snapshot()
	if snap["good-repo"].LastError != "" {
		t.Errorf("good-repo: got error %q, want none", snap["good-repo"].LastError)
	}
	if snap["good-repo"].LastSyncAt.IsZero() {
		t.Error("good-repo: expected LastSyncAt to be set")
	}
	if snap["bad-repo"].LastError != boom.Error() {
		t.Errorf("bad-repo: LastError = %q, want %q", snap["bad-repo"].LastError, boom.Error())
	}
}

// --- syncAll / syncRepos ---

func TestSyncAll_CallsSyncerForEveryRepoAndTracksFailures(t *testing.T) {
	tr := newRepoSyncTracker()
	var seen []string
	syncer := tr.wrap(func(_ context.Context, repo config.RepoConfig) error {
		seen = append(seen, repo.Name)
		if repo.Name == "flaky" {
			return errors.New("fetch failed")
		}
		return nil
	})

	repos := []config.RepoConfig{{Name: "app"}, {Name: "flaky"}, {Name: "platform"}}
	syncAll(context.Background(), repos, syncer)

	if len(seen) != 3 {
		t.Fatalf("expected all 3 repos synced, got %v", seen)
	}
	snap := tr.snapshot()
	if snap["flaky"].LastError == "" {
		t.Error("expected 'flaky' to have a recorded error")
	}
	if snap["app"].LastError != "" || snap["platform"].LastError != "" {
		t.Errorf("expected 'app'/'platform' to have no error, got %+v", snap)
	}
}

func TestSyncRepos_ExitsOnCtxDone(t *testing.T) {
	var calls int
	var mu sync.Mutex
	syncer := func(_ context.Context, _ config.RepoConfig) error { //nolint:unparam // signature fixed by syncRepos' syncer parameter
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		syncRepos(ctx, []config.RepoConfig{{Name: "app"}}, 10*time.Millisecond, syncer)
		close(done)
	}()

	// Let the immediate startup sync (and likely at least one ticker fire)
	// happen before canceling.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("syncRepos did not return within 500ms of ctx cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if calls == 0 {
		t.Error("expected at least the immediate startup sync to have run")
	}
}
