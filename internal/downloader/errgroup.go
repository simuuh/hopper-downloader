package downloader

import (
	"context"
	"sync"
)

// group is a minimal, local stand-in for golang.org/x/sync/errgroup: run a
// fixed set of goroutines, cancel a shared context on the first error, and
// report that first error from Wait. Kept in-package to avoid an extra
// dependency for what is a handful of lines.
type group struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	firstErr error
}

func newErrGroup(ctx context.Context) (*group, context.Context) {
	gctx, cancel := context.WithCancel(ctx)
	return &group{ctx: gctx, cancel: cancel}, gctx
}

func (g *group) Go(fn func() error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := fn(); err != nil {
			g.mu.Lock()
			if g.firstErr == nil {
				g.firstErr = err
				g.cancel()
			}
			g.mu.Unlock()
		}
	}()
}

func (g *group) Wait() error {
	g.wg.Wait()
	g.cancel()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.firstErr
}
