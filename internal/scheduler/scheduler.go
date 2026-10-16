// Package scheduler provides small, dependency-free primitives for
// running functions periodically or at a specific future time, without
// any package-level state. It is used by internal/queue to drive its
// dispatch loop and to wake precisely when a "start at time X" item
// becomes due.
package scheduler

import (
	"context"
	"sync"
	"time"
)

// Scheduler tracks goroutines started via Every/At so callers can wait for
// them to finish (e.g. during graceful shutdown, after cancelling their
// context).
type Scheduler struct {
	wg sync.WaitGroup
}

// New returns an empty Scheduler.
func New() *Scheduler {
	return &Scheduler{}
}

// Every runs fn once per interval until ctx is cancelled. The first run
// happens after the first interval elapses, not immediately.
func (s *Scheduler) Every(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fn(ctx)
			}
		}
	}()
}

// At runs fn once at time t (immediately if t is already in the past), or
// not at all if ctx is cancelled first.
func (s *Scheduler) At(ctx context.Context, t time.Time, fn func(context.Context)) {
	d := time.Until(t)
	if d < 0 {
		d = 0
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			fn(ctx)
		}
	}()
}

// Wait blocks until every function started via Every/At has returned.
// Callers should cancel the context(s) passed to Every/At first, or Wait
// may block indefinitely on an Every loop.
func (s *Scheduler) Wait() {
	s.wg.Wait()
}
