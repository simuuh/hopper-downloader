package downloader

import (
	"sync/atomic"
	"time"
)

const progressInterval = 250 * time.Millisecond

// startProgressReporter polls st for total bytes downloaded at a fixed
// interval, invoking onProgress (if non-nil) with cumulative progress and
// an instantaneous speed estimate. It always tracks the peak of those
// instantaneous samples — regardless of whether onProgress is set — so
// Download can report Result.PeakSpeed even for a caller that doesn't
// pass OnProgress. Runs until the returned stop function is called, at
// which point it emits one final sample and returns the peak observed.
func startProgressReporter(st *state, totalSize int64, onProgress func(ProgressUpdate)) (stop func() int64) {
	done := make(chan struct{})
	stopped := make(chan struct{})
	var peak atomic.Int64

	go func() {
		defer close(stopped)
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()

		last := st.totalDownloaded()
		lastAt := time.Now()

		emit := func() {
			now := time.Now()
			cur := st.totalDownloaded()
			elapsed := now.Sub(lastAt).Seconds()
			var speed int64
			if elapsed > 0 {
				speed = int64(float64(cur-last) / elapsed)
			}
			if speed > peak.Load() {
				peak.Store(speed)
			}
			if onProgress != nil {
				total := totalSize
				if total <= 0 {
					total = -1
				}
				onProgress(ProgressUpdate{BytesDownloaded: cur, TotalBytes: total, Speed: speed})
			}
			last, lastAt = cur, now
		}

		for {
			select {
			case <-ticker.C:
				emit()
			case <-done:
				emit()
				return
			}
		}
	}()

	return func() int64 {
		close(done)
		<-stopped
		return peak.Load()
	}
}
