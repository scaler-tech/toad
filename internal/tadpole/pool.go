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
}

// NewPool creates a tadpole pool with the given semaphore and runner.
func NewPool(sem chan struct{}, runner *Runner) *Pool {
	return &Pool{sem: sem, runner: runner}
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

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.sem }()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("tadpole panicked", "error", r, "task", task.Summary)
			}
		}()

		if err := p.runner.Execute(ctx, task); err != nil {
			slog.Error("tadpole failed", "error", err, "task", task.Summary)
		}
	}()

	return nil
}

// Shutdown waits for all running tadpoles to finish with a 30-second grace period.
func (p *Pool) Shutdown(ctx context.Context) {
	slog.Info("shutting down tadpole pool, waiting for active runs...")
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	grace := 30 * time.Second
	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-done:
		slog.Info("all tadpoles finished")
	case <-timer.C:
		slog.Warn("tadpole shutdown timed out after grace period", "grace", grace)
	case <-ctx.Done():
		slog.Warn("tadpole shutdown cancelled")
	}
}
