package downloader

import "golang.org/x/time/rate"

func newByteLimiter(bytesPerSec int64) *rate.Limiter {
	if bytesPerSec <= 0 {
		return nil
	}
	// Burst = one second's worth, at least minLimitedRead so a very low
	// limit still makes progress in reasonable steps. fetchChunkOnce never
	// reads more than a limiter's burst at once (readSize), so WaitN can
	// always be satisfied.
	burst := int(bytesPerSec)
	if burst < minLimitedRead {
		burst = minLimitedRead
	}
	return rate.NewLimiter(rate.Limit(bytesPerSec), burst)
}

// hosterLimiter returns the shared bandwidth limiter for a hoster key,
// lazily created from the engine's configured per-hoster limits. Returns
// nil if key is empty or has no configured limit.
func (e *Engine) hosterLimiter(key string) *rate.Limiter {
	if key == "" {
		return nil
	}
	limit, ok := e.hosterBWLimits[key]
	if !ok || limit <= 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if l, ok := e.hosterLimiters[key]; ok {
		return l
	}
	l := newByteLimiter(limit)
	e.hosterLimiters[key] = l
	return l
}

// hosterGate returns the per-hoster "minimum interval between download
// sessions" limiter, used to respect free-user wait times advertised by a
// hoster. It is consulted once per Download call, not per chunk.
func (e *Engine) hosterGate(key string) *rate.Limiter {
	if key == "" {
		return nil
	}
	interval, ok := e.hosterMinInterval[key]
	if !ok || interval <= 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if g, ok := e.hosterGates[key]; ok {
		return g
	}
	g := rate.NewLimiter(rate.Every(interval), 1)
	e.hosterGates[key] = g
	return g
}

// minLimitedRead is the smallest read size (and limiter burst) used while
// a bandwidth limit is active.
const minLimitedRead = 64 * 1024

// readSize returns how many bytes one read may take: the full buffer when
// unthrottled, else no more than the smallest active limiter's burst —
// WaitN rejects n > burst, and smaller steps keep a throttled transfer's
// progress smooth instead of arriving in one-second lumps.
func readSize(bufLen int, limiters ...*rate.Limiter) int {
	n := bufLen
	for _, l := range limiters {
		if l == nil {
			continue
		}
		if b := l.Burst(); b > 0 && b < n {
			n = b
		}
	}
	return n
}
