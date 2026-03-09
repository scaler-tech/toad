package tadpole

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Pool manages concurrent tadpole execution with a semaphore.
type Pool struct {
	sem    chan struct{}
	runner *Runner
	wg     sync.WaitGroup

	mu      sync.Mutex
	nextID  uint64
	cancels map[uint64]context.CancelFunc
}

// NewPool creates a tadpole pool with the given semaphore and runner.
func NewPool(sem chan struct{}, runner *Runner) *Pool {
	return &Pool{sem: sem, runner: runner, cancels: make(map[uint64]context.CancelFunc)}
}

// Spawn acquires a semaphore slot and launches a tadpole in a goroutine.
// Blocks if all slots are in use.
func (p *Pool) Spawn(ctx context.Context, task Task) error {
	// Acquire semaphore (blocks if full)
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Detach from the parent context so running tadpoles aren't killed
	// when the signal context cancels (shutdown/restart). Each tadpole
	// gets its own cancel func so Shutdown can force-stop them after
	// the grace period expires.
	execCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	p.mu.Lock()
	id := p.nextID
	p.nextID++
	p.cancels[id] = cancel
	p.mu.Unlock()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.sem }()
		defer func() {
			cancel()
			p.mu.Lock()
			delete(p.cancels, id)
			p.mu.Unlock()
		}()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("tadpole panicked", "error", r, "task", task.Summary)
			}
		}()

		if err := p.runner.Execute(execCtx, task); err != nil {
			slog.Error("tadpole failed", "error", err, "task", task.Summary)
		}
	}()

	return nil
}

// Shutdown waits for all running tadpoles to finish with a grace period.
// Pass a longer grace period for restart (where you want work to complete)
// vs a short one for hard shutdown. After the grace period, remaining
// tadpoles are forcefully canceled.
func (p *Pool) Shutdown(grace time.Duration) {
	slog.Info("shutting down tadpole pool, waiting for active runs...", "grace", grace)
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-done:
		slog.Info("all tadpoles finished")
	case <-timer.C:
		slog.Warn("tadpole shutdown timed out, canceling remaining runs", "grace", grace)
		p.mu.Lock()
		for _, cancel := range p.cancels {
			cancel()
		}
		p.mu.Unlock()
		// Give canceled tadpoles a moment to clean up
		select {
		case <-done:
			slog.Info("all tadpoles finished after cancel")
		case <-time.After(10 * time.Second):
			slog.Warn("some tadpoles did not exit after cancel")
		}
	}
}
