package agent

import (
	"context"
	"sync"
)

// MockProvider is a test double that records calls and returns configurable results.
type MockProvider struct {
	mu sync.Mutex

	// RunResult is returned by Run. Set before calling the code under test.
	// If RunResults is set, it takes precedence and results are returned sequentially.
	RunResult *RunResult
	// RunResults returns results sequentially; takes precedence over RunResult.
	RunResults []*RunResult
	// RunErr is returned as the error from Run.
	RunErr error

	// RunCalls records every RunOpts passed to Run, in order.
	RunCalls []RunOpts
	// runCallCount tracks which RunResults entry to return next
	runCallCount int
}

func (m *MockProvider) Check() error { return nil }

func (m *MockProvider) Run(_ context.Context, opts RunOpts) (*RunResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RunCalls = append(m.RunCalls, opts)

	// Use RunResults if set, otherwise use RunResult
	if len(m.RunResults) > 0 {
		if m.runCallCount < len(m.RunResults) {
			result := m.RunResults[m.runCallCount]
			m.runCallCount++
			return result, m.RunErr
		}
		// If we've exhausted RunResults, return nil
		return nil, m.RunErr
	}
	return m.RunResult, m.RunErr
}

// LastRunOpts returns the RunOpts from the most recent Run call, or zero value if none.
func (m *MockProvider) LastRunOpts() RunOpts {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.RunCalls) == 0 {
		return RunOpts{}
	}
	return m.RunCalls[len(m.RunCalls)-1]
}
