package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"
)

// downloadChunk downloads a single chunk with retry and exponential
// backoff, resuming from st.Chunks[idx].Downloaded (if resumable) on each
// retry.
func (e *Engine) downloadChunk(ctx context.Context, f *os.File, req Request, st *state, idx int) error {
	var lastErr error
	for attempt := 0; attempt <= int(e.retryMax.Load()); attempt++ {
		if attempt > 0 {
			delay := backoffDelay(e.retryBackoffBase, attempt)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		// Without Range support every attempt receives the file from byte
		// 0, so its progress must start there too.
		if !st.Resumable {
			st.resetChunk(idx)
		}

		err := e.fetchChunkOnce(ctx, f, req, st, idx)
		if err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		var perm *permanentError
		if errors.As(err, &perm) {
			return fmt.Errorf("downloader: chunk %d: %w", idx, perm.err)
		}
		lastErr = err
	}
	return fmt.Errorf("downloader: chunk %d failed after %d attempts: %w", idx, e.retryMax.Load()+1, lastErr)
}

func (e *Engine) fetchChunkOnce(ctx context.Context, f *os.File, req Request, st *state, idx int) error {
	chunk := st.Chunks[idx]
	start := chunk.Start + st.chunkDownloaded(idx)
	if chunk.End >= chunk.Start && start > chunk.End {
		return nil // already complete
	}

	// Stall watchdog: if no data arrives for stallTimeout, cancel this
	// request with errStalled. The caller's retry loop then resumes from
	// the bytes already written instead of waiting on a dead connection
	// forever.
	reqCtx, cancelReq := context.WithCancelCause(ctx)
	defer cancelReq(nil)
	watchdog := time.AfterFunc(e.stallTimeout, func() { cancelReq(errStalled) })
	defer watchdog.Stop()
	stalled := func(err error) error {
		if ctx.Err() == nil && errors.Is(context.Cause(reqCtx), errStalled) {
			return fmt.Errorf("%w: no data for %s", errStalled, e.stallTimeout)
		}
		return err
	}

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, req.URL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if st.Resumable {
		if chunk.End >= chunk.Start {
			httpReq.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, chunk.End))
		} else {
			httpReq.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
		}
	}

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return stalled(fmt.Errorf("request failed: %w", err))
	}
	defer resp.Body.Close()

	// A chunk that is already complete returns before any request is made,
	// so a 416 always means bytes are missing — the file behind the URL
	// shrank or is no longer served. Retrying the same range won't help.
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return &permanentError{fmt.Errorf("server cannot serve bytes %d- (remote file changed?)", start)}
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	// An expired/invalid download link very often still answers 200 —
	// with the hoster's HTML error or login page. Saving that as
	// "movie.part1.rar" only surfaces much later as a confusing
	// extraction failure, so fail now with a clear, non-retried error.
	if isHTML(resp.Header.Get("Content-Type")) && !looksLikeHTMLFile(req.RelPath) {
		return &permanentError{errors.New("hoster returned an HTML page instead of the file (download link expired, or account/premium required?)")}
	}

	// A server that ignores our Range header answers 200 with the whole
	// file. Writing that body at this chunk's offset would silently
	// corrupt the output, so it's only acceptable when we asked for the
	// file from byte 0 anyway; otherwise fail permanently — retrying the
	// same request would just get the same full body again.
	if st.Resumable && resp.StatusCode == http.StatusOK && start != 0 {
		return &permanentError{fmt.Errorf("server ignored range request (got 200 for bytes=%d-)", start)}
	}

	var body io.Reader = resp.Body
	var want int64 = -1 // bytes this request must deliver, -1 = unknown
	if chunk.End >= chunk.Start {
		want = chunk.End - start + 1
		// Never write past this chunk's range, even if a misbehaving
		// server sends more than requested (or the full file on a 200).
		body = io.LimitReader(resp.Body, want)
	}

	buf := make([]byte, readBufSize)
	offset := start
	hoster := e.hosterLimiter(req.HosterKey)
	for {
		// Loaded per read, not once per request: a bandwidth change from
		// the Web UI (SetGlobalBandwidthLimit swaps the pointer) must reach
		// transfers already in flight, not only the next chunk — which for
		// one big file may never come.
		global := e.globalLimiter.Load()
		n, readErr := body.Read(buf[:readSize(len(buf), global, hoster)])
		if n > 0 {
			watchdog.Reset(e.stallTimeout)
			if global != nil {
				if werr := global.WaitN(ctx, n); werr != nil {
					return werr
				}
			}
			if hoster != nil {
				if werr := hoster.WaitN(ctx, n); werr != nil {
					return werr
				}
			}
			if _, werr := f.WriteAt(buf[:n], offset); werr != nil {
				return fmt.Errorf("write: %w", werr)
			}
			offset += int64(n)
			st.addProgress(idx, int64(n))
			if err := st.saveThrottled(); err != nil {
				return fmt.Errorf("persist resume state: %w", err)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				// A clean EOF before the chunk's range is complete is a
				// dropped connection, not success — hosters do this under
				// load. Returning an error makes the retry loop resume
				// from the bytes already written instead of finalizing a
				// truncated file as "done".
				if got := offset - start; want >= 0 && got < want {
					return fmt.Errorf("connection closed early: got %d of %d bytes", got, want)
				}
				return nil
			}
			return stalled(fmt.Errorf("read: %w", readErr))
		}
	}
}

const maxBackoff = 60 * time.Second

func backoffDelay(base time.Duration, attempt int) time.Duration {
	d := float64(base) * math.Pow(2, float64(attempt-1))
	if d > float64(maxBackoff) {
		d = float64(maxBackoff)
	}
	jitter := 0.85 + rand.Float64()*0.3 //nolint:gosec // G404: retry-delay jitter, not a security context
	return time.Duration(d * jitter)
}

// permanentError marks a chunk failure that retrying the identical
// request cannot fix (e.g. a server that ignores Range headers), so
// downloadChunk gives up immediately instead of burning its retry budget
// and backoff delays on it.
type permanentError struct{ err error }

func (p *permanentError) Error() string { return p.err.Error() }
func (p *permanentError) Unwrap() error { return p.err }

func isHTML(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "application/xhtml")
}

func looksLikeHTMLFile(relPath string) bool {
	p := strings.ToLower(relPath)
	return strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".htm") || strings.HasSuffix(p, ".xhtml")
}

// errStalled marks a transfer aborted by the stall watchdog — retryable,
// unlike a cancellation of the caller's own context.
var errStalled = errors.New("transfer stalled")
