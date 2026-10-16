package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestEvery_RunsRepeatedlyUntilCancelled(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())

	var calls int32
	s.Every(ctx, 10*time.Millisecond, func(context.Context) {
		atomic.AddInt32(&calls, 1)
	})

	// Wait for repeated calls rather than sleeping a fixed 55ms and counting:
	// Windows' ~15.6ms timer granularity (worse under load) made a fixed
	// window flaky without saying anything about correctness.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&calls) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	s.Wait()

	got := atomic.LoadInt32(&calls)
	if got < 3 {
		t.Fatalf("expected at least 3 repeated calls, got %d", got)
	}
	time.Sleep(50 * time.Millisecond)
	if after := atomic.LoadInt32(&calls); after != got {
		t.Errorf("calls kept running after cancel: %d -> %d", got, after)
	}
}

func TestAt_RunsOnceAtFutureTime(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan time.Time, 1)
	start := time.Now()
	s.At(ctx, start.Add(30*time.Millisecond), func(context.Context) {
		done <- time.Now()
	})

	select {
	case fired := <-done:
		if elapsed := fired.Sub(start); elapsed < 20*time.Millisecond {
			t.Errorf("fired too early: %v", elapsed)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("At did not fire within 200ms")
	}
	s.Wait()
}

func TestAt_PastTimeRunsImmediately(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{}, 1)
	s.At(ctx, time.Now().Add(-time.Hour), func(context.Context) {
		close(done)
	})

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("At with past time did not run promptly")
	}
	s.Wait()
}

func TestAt_CancelledBeforeFiringNeverRuns(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())

	var ran int32
	s.At(ctx, time.Now().Add(time.Hour), func(context.Context) {
		atomic.StoreInt32(&ran, 1)
	})
	cancel()
	s.Wait()

	if atomic.LoadInt32(&ran) != 0 {
		t.Error("At fn ran despite context being cancelled before its time")
	}
}
