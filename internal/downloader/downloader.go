// Package downloader implements Hopper's HTTP(S) transfer engine: chunked
// parallel downloads with resume via Range requests, retry with
// exponential backoff, and global/per-hoster bandwidth limiting.
//
// The engine is deliberately hoster-agnostic: it downloads from an
// already-resolved direct URL. Turning a hoster page URL into that direct
// URL (via a plugins.Hoster) is the caller's job (internal/queue), which
// keeps this package testable against a plain httptest server.
package downloader

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/simuuh/hopper-downloader/internal/pathsafe"
)

// readBufSize is the largest single read+write per chunk. 1 MiB rather than
// the classic 32 KiB: measured locally, 32 KiB capped the engine at
// ~40–80 MB/s (one WriteAt syscall and one progress-mutex round per 32
// KiB), 1 MiB reached ~90–105 MB/s — enough for a saturated gigabit line.
// With a bandwidth limit active, reads are capped to the limiter's burst
// instead (see readSize) so throttled transfers stay smooth.
const readBufSize = 1024 * 1024

// minChunkSize is the smallest range the engine splits a file into when
// the caller didn't request an explicit chunk count.
const minChunkSize = 4 << 20 // 4 MiB

// ProgressUpdate reports incremental transfer progress for a single
// download. Speed is an instantaneous estimate in bytes/sec.
type ProgressUpdate struct {
	BytesDownloaded int64
	TotalBytes      int64 // -1 if unknown
	Speed           int64
}

// Request describes a single file to download.
type Request struct {
	// URL is the direct, already-resolved download URL.
	URL string
	// RelPath is the destination path relative to the engine's BaseDir,
	// e.g. "package-name/file.zip". It is resolved via pathsafe.SafeJoin
	// against BaseDir, so it can never escape BaseDir.
	RelPath string
	// ExpectedSize is the size in bytes if already known (e.g. from a
	// prior plugins.Hoster.Resolve call), or 0 if unknown.
	ExpectedSize int64
	// ParallelChunks overrides the engine default for this request; 0
	// uses the engine default.
	ParallelChunks int
	// HosterKey selects which per-hoster bandwidth/rate limit applies;
	// empty means "no per-hoster limit".
	HosterKey string
	// OnProgress, if set, is called at a bounded rate (a few times per
	// second) with cumulative progress.
	OnProgress func(ProgressUpdate)
}

// Result summarizes a completed download.
type Result struct {
	Path         string
	BytesWritten int64
	Resumed      bool
	// Skipped is true when RelPath already existed as a complete file
	// and nothing was actually transferred — see Download's destination
	// check. BytesWritten still reflects the existing file's real size
	// in this case, not 0.
	Skipped bool
	// PeakSpeed is the highest instantaneous speed sample (bytes/sec)
	// observed during the transfer — see startProgressReporter. 0 for a
	// Skipped result, since no transfer happened to sample.
	PeakSpeed int64
}

// Config configures an Engine.
type Config struct {
	BaseDir                  string
	Client                   *http.Client
	DefaultParallelChunks    int
	RetryMax                 int
	RetryBackoffBase         time.Duration
	GlobalBandwidthLimit     int64            // bytes/sec, 0 = unlimited
	HosterBandwidthLimits    map[string]int64 // bytes/sec, per HosterKey
	HosterMinRequestInterval map[string]time.Duration
	// StallTimeout aborts (and then resumes) a transfer that received no
	// data for this long — a hoster that stops sending without closing
	// the connection would otherwise hang the download forever. Default
	// 60s.
	StallTimeout time.Duration
}

// Engine executes download Requests. An Engine is safe for concurrent use;
// callers typically run one Download call per queue worker goroutine.
//
// defaultChunks, retryMax, and globalLimiter are all live-adjustable (see
// SetDefaultParallelChunks, SetRetryMax, SetGlobalBandwidthLimit — used
// by internal/settings so a Web UI change takes effect without a
// restart) and are therefore accessed atomically rather than as plain
// fields: downloadChunk and Download read them from arbitrary worker
// goroutines while a settings update can write them concurrently from
// the API's goroutine. baseDir joins that set for the same reason (see
// SetBaseDir) — it used to be a plain field, fine only as long as
// nothing could change it after construction.
type Engine struct {
	baseDir           atomic.Pointer[string]
	client            *http.Client
	defaultChunks     atomic.Int64
	retryMax          atomic.Int64
	retryBackoffBase  time.Duration
	stallTimeout      time.Duration
	globalLimiter     atomic.Pointer[rate.Limiter] // nil = unlimited
	hosterBWLimits    map[string]int64
	hosterMinInterval map[string]time.Duration

	mu             sync.Mutex
	hosterLimiters map[string]*rate.Limiter
	hosterGates    map[string]*rate.Limiter
}

// New builds an Engine from Config, applying sane defaults for any zero
// value.
func New(cfg Config) *Engine {
	client := cfg.Client
	if client == nil {
		// No overall timeout (a large file legitimately takes hours); a slow
		// or silent server is caught by ResponseHeaderTimeout and the
		// per-chunk stall watchdog (StallTimeout) instead.
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 30 * time.Second
		client = &http.Client{Transport: transport}
	}
	chunks := cfg.DefaultParallelChunks
	if chunks < 1 {
		chunks = 4
	}
	retryMax := cfg.RetryMax
	if retryMax < 0 {
		retryMax = 0
	}
	stall := cfg.StallTimeout
	if stall <= 0 {
		stall = 60 * time.Second
	}
	backoff := cfg.RetryBackoffBase
	if backoff <= 0 {
		backoff = 2 * time.Second
	}

	e := &Engine{
		client:            client,
		retryBackoffBase:  backoff,
		stallTimeout:      stall,
		hosterBWLimits:    cfg.HosterBandwidthLimits,
		hosterMinInterval: cfg.HosterMinRequestInterval,
		hosterLimiters:    make(map[string]*rate.Limiter),
		hosterGates:       make(map[string]*rate.Limiter),
	}
	e.baseDir.Store(&cfg.BaseDir)
	e.defaultChunks.Store(int64(chunks))
	e.retryMax.Store(int64(retryMax))
	e.globalLimiter.Store(newByteLimiter(cfg.GlobalBandwidthLimit))
	return e
}

// BaseDir returns the directory downloaded files are written under —
// used by internal/queue's post-download archive extraction to resolve
// a completed item's (or a multi-part archive's first part's) absolute
// path on disk from its RelPath.
func (e *Engine) BaseDir() string { return *e.baseDir.Load() }

// SetBaseDir changes the directory new downloads are written under, live
// — internal/settings.Settings.DownloadPath's Hooks callback (see
// cmd/hopperd/main.go) calls this on a Web UI change, so it applies
// immediately rather than only on the next hopperd restart. Swapped
// wholesale (a fresh string pointer, not mutated in place) for the same
// reason globalLimiter is: Download reads BaseDir() from arbitrary
// worker goroutines while this can be called concurrently from the
// API's goroutine. A download already in progress keeps resolving
// against whatever BaseDir it started with (it already computed its
// absolute destination path up front); only downloads that start after
// this call see the new directory.
func (e *Engine) SetBaseDir(dir string) {
	e.baseDir.Store(&dir)
}

// SetDefaultParallelChunks changes the default chunk count for requests
// that don't set Request.ParallelChunks explicitly. Applies to the next
// Download call; a transfer already in progress keeps the chunk count it
// started with.
func (e *Engine) SetDefaultParallelChunks(n int) {
	if n < 1 {
		n = 1
	}
	e.defaultChunks.Store(int64(n))
}

// SetRetryMax changes the per-chunk retry budget live. downloadChunk
// reads it fresh on every iteration of its retry loop, so this can even
// extend or cut short a chunk that's already mid-retry.
func (e *Engine) SetRetryMax(n int) {
	if n < 0 {
		n = 0
	}
	e.retryMax.Store(int64(n))
}

// SetGlobalBandwidthLimit changes the engine-wide bandwidth cap live.
// bytesPerSec <= 0 means unlimited. Swaps in a fresh *rate.Limiter
// (built the same way New does) rather than mutating the existing one
// in place, so concurrent readers (fetchChunkOnce, via
// globalLimiter.Load()) never observe a partially-updated limiter; an
// in-flight WaitN call on the old limiter simply finishes under the old
// rate; every new one after this call sees the new rate immediately.
func (e *Engine) SetGlobalBandwidthLimit(bytesPerSec int64) {
	e.globalLimiter.Store(newByteLimiter(bytesPerSec))
}

// Download fetches req.URL to BaseDir/req.RelPath, resuming a previous
// partial attempt if one is found and the remote file is still the same
// version (see loadOrCreateState). It blocks until the transfer completes,
// fails permanently (after retryMax attempts per chunk), or ctx is
// cancelled.
func (e *Engine) Download(ctx context.Context, req Request) (*Result, error) {
	dest, err := pathsafe.SafeJoin(e.BaseDir(), req.RelPath)
	if err != nil {
		return nil, fmt.Errorf("downloader: resolve destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return nil, fmt.Errorf("downloader: create destination directory: %w", err)
	}

	if gate := e.hosterGate(req.HosterKey); gate != nil {
		if err := gate.Wait(ctx); err != nil {
			return nil, fmt.Errorf("downloader: hoster rate gate: %w", err)
		}
	}

	// A destination that already exists with no in-progress resume state
	// alongside it is a completed prior download (or a file placed there
	// out of band) — not an error, and not something to fetch again:
	// skip the transfer entirely and report the existing file back as a
	// success (Result.Skipped) rather than failing the request. Callers
	// that want failure-on-conflict semantics should check RelPath
	// themselves before calling Download.
	//
	// Exception: an existing file clearly smaller than the size the hoster
	// reported (ExpectedSize) is a truncated leftover, e.g. from a crash or
	// an older build that finalized short transfers — re-download it
	// instead of "skipping" a broken file forever. The 1% tolerance absorbs
	// hoster APIs that report rounded sizes.
	if info, statErr := os.Stat(dest); statErr == nil {
		if _, partErr := os.Stat(statePath(dest)); partErr != nil {
			if req.ExpectedSize <= 0 || info.Size() >= req.ExpectedSize-req.ExpectedSize/100 {
				return &Result{Path: dest, BytesWritten: info.Size(), Skipped: true}, nil
			}
		}
	}

	res, err := e.transfer(ctx, req, dest)
	if errors.Is(err, errRemoteChanged) {
		// The file behind the URL was replaced mid-transfer: the bytes on
		// disk belong to the old version. Start over once against the new
		// one (the fresh start truncates the .part); a second change in a
		// row is reported as an error.
		if rmErr := os.Remove(statePath(dest)); rmErr != nil && !os.IsNotExist(rmErr) {
			return nil, fmt.Errorf("downloader: discard outdated resume state: %w", rmErr)
		}
		res, err = e.transfer(ctx, req, dest)
	}
	return res, err
}

// transfer runs one probe-and-download pass of req into dest, resuming
// from the saved state when it still matches the remote file.
func (e *Engine) transfer(ctx context.Context, req Request, dest string) (*Result, error) {
	remote, probeErr := probe(ctx, e.client, req.URL)
	if probeErr != nil && req.ExpectedSize <= 0 {
		return nil, fmt.Errorf("downloader: probe %s: %w", redactURL(req.URL), probeErr)
	}
	size, supportsRange := remote.size, remote.ranges
	// chunkSize is the length the chunk ranges (and therefore the
	// "connection closed early" check in fetchChunkOnce) are built from.
	// Only a size the HTTP probe itself reported is trusted for that; a
	// hoster-API-reported ExpectedSize is still used for progress display,
	// but may be rounded, so a transfer is never failed for not matching it.
	chunkSize := size
	if size <= 0 && req.ExpectedSize > 0 {
		size = req.ExpectedSize
	}
	resumable := supportsRange && chunkSize > 0

	numChunks := req.ParallelChunks
	if numChunks < 1 {
		numChunks = int(e.defaultChunks.Load())
		// Splitting a small file into many ranged requests only adds
		// round-trips (and hoster connection-limit pressure) without any
		// throughput gain; keep each default chunk at least minChunkSize.
		if chunkSize > 0 {
			if maxChunks := int(chunkSize / minChunkSize); numChunks > maxChunks {
				numChunks = max(maxChunks, 1)
			}
		}
	}
	if !resumable {
		numChunks = 1
	}

	st, resumed := loadOrCreateState(dest, req.URL, chunkSize, resumable, numChunks, remote.validator)
	st.path = statePath(dest)

	tmp := tempPath(dest)
	// Resume state without its data file (deleted by hand, or lost on a
	// crash before the first write hit disk) would make WriteAt fill the
	// "already downloaded" ranges with zeros — start over instead.
	if resumed {
		if _, err := os.Stat(tmp); err != nil {
			st, resumed = newState(req.URL, chunkSize, resumable, numChunks, statePath(dest)), false
			st.Validator = remote.validator
		}
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("downloader: open temp file: %w", err)
	}
	defer func() { _ = f.Close() }()
	// A fresh start may find a .part left over from another URL, size or
	// an aborted attempt; its bytes beyond what this transfer writes would
	// otherwise end up in the finished file.
	if !resumed {
		if err := f.Truncate(0); err != nil {
			return nil, fmt.Errorf("downloader: reset temp file: %w", err)
		}
	}

	stopProgress := startProgressReporter(st, size, req.OnProgress)

	g, gctx := newErrGroup(ctx)
	for i := range st.Chunks {
		i := i
		g.Go(func() error {
			return e.downloadChunk(gctx, f, req, st, i)
		})
	}

	waitErr := g.Wait()
	peakSpeed := stopProgress()

	if waitErr != nil {
		_ = st.save()
		return nil, waitErr
	}

	// Last line of defense against finalizing a file with a hole: every
	// chunk reporting success must add up to the full remote size.
	if got := st.totalDownloaded(); st.Size > 0 && got != st.Size {
		_ = st.save()
		return nil, fmt.Errorf("downloader: incomplete: received %d of %d bytes", got, st.Size)
	}

	// The finished file is exactly as long as the remote file (or, with an
	// unknown size, as what was received) — never longer, whatever the
	// .part held before.
	finalSize := st.Size
	if finalSize <= 0 {
		finalSize = st.totalDownloaded()
	}
	if err := f.Truncate(finalSize); err != nil {
		return nil, fmt.Errorf("downloader: set final file size: %w", err)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("downloader: sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("downloader: close temp file: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return nil, fmt.Errorf("downloader: finalize file: %w", err)
	}
	_ = os.Remove(statePath(dest))

	return &Result{Path: dest, BytesWritten: st.totalDownloaded(), Resumed: resumed, PeakSpeed: peakSpeed}, nil
}

// DiscardPartial removes the in-progress data and resume state of an
// unfinished download of relPath (its .part and .part.json files) — used
// when the user removes an item that never completed, so the partial
// data doesn't stay behind in the downloads folder. A completed file is
// never touched. Missing files are not an error.
func (e *Engine) DiscardPartial(relPath string) error {
	dest, err := pathsafe.SafeJoin(e.BaseDir(), relPath)
	if err != nil {
		return fmt.Errorf("downloader: resolve destination: %w", err)
	}
	var errs []error
	for _, p := range []string{tempPath(dest), statePath(dest)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
