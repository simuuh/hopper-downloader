package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/simuuh/hopper-downloader/internal/downloader"
	"github.com/simuuh/hopper-downloader/internal/extract"
	"github.com/simuuh/hopper-downloader/internal/pathsafe"
	"github.com/simuuh/hopper-downloader/internal/scheduler"
	"github.com/simuuh/hopper-downloader/plugins"
)

// ErrNotFound is returned by Manager methods that operate on an item or
// package ID that doesn't exist.
var ErrNotFound = errors.New("queue: not found")

// extractTimeout bounds a single archive extraction. Generous — a large
// multi-part RAR set can legitimately take a while — but extraction
// running forever (e.g. unrar hanging on a truly corrupt archive despite
// -p-) shouldn't be able to leak a goroutine permanently.
const extractTimeout = 30 * time.Minute

// AccountLookup resolves a hoster key to credentials. internal/account's
// Manager satisfies this implicitly; queue depends only on the interface
// so it stays testable and doesn't need to import account.
type AccountLookup interface {
	AccountFor(hosterKey string) (*plugins.Account, bool)
}

// AccountFailureReporter is optionally implemented by the AccountLookup
// (internal/account.Manager does): run() reports an account the hoster
// just rejected (plugins.ErrAccountRequired) so the next AccountFor call
// for that hoster hands out a different account — see rotateAccount.
type AccountFailureReporter interface {
	ReportFailure(accountID string) bool
}

// Extractor performs post-download archive extraction.
// internal/extract.Extractor satisfies this implicitly; queue depends
// only on the interface so it stays testable without shelling out to a
// real unrar binary.
type Extractor interface {
	Extract(ctx context.Context, srcPath, destDir, pkgPassword string, userPasswords []string) (*extract.Result, error)
}

// StatsRecorder records completed/failed downloads for the stats
// dashboard. internal/stats.Tracker satisfies this implicitly; queue
// depends only on the interface, same reasoning as AccountLookup/
// Extractor above. May be nil (as in most existing tests), which simply
// disables stats recording — a Manager has no other behavior that
// depends on it.
type StatsRecorder interface {
	RecordDownload(hosterKey string, bytes int64, speed int64)
	RecordError(hosterKey string)
}

// ExtractSettings resolves the currently-effective archive-extraction
// configuration. Read fresh on every item completion rather than pushed
// via a callback (unlike e.g. SetParallelFiles): extraction only happens
// at discrete completion events, so there's nothing to "push into"
// between them. internal/settings.Manager satisfies this implicitly via
// a small adapter in cmd/hopperd/main.go — depending on the interface
// (not internal/settings) keeps this package testable without a real
// bbolt-backed settings store.
type ExtractSettings interface {
	ExtractEnabled() bool
	ExtractDeleteAfter() bool
	ExtractPasswords() []string
}

// Config configures a Manager.
type Config struct {
	// ParallelFiles caps concurrently downloading items.
	ParallelFiles int
	// PollInterval is the fallback interval at which the dispatcher
	// re-scans for runnable items (queued items becoming due, or a slot
	// freeing up). Explicit mutations also wake the dispatcher
	// immediately; this is a safety net, not the primary trigger.
	PollInterval time.Duration
	// DiskCheckAttempts/DiskCheckInterval override how long runExtraction
	// waits for every archive part to actually be visible on disk before
	// giving up (see the diskCheckAttempts/diskCheckInterval package
	// vars' doc comment) — sourced from config.ExtractConfig's
	// part_wait_attempts/part_wait_interval. Zero (the default for any
	// caller that doesn't set them, e.g. every existing test) falls back
	// to those package-level defaults.
	DiskCheckAttempts int
	DiskCheckInterval time.Duration
	// SequentialParts keeps a multi-part archive's parts from overtaking
	// each other: a part only starts once every lower-numbered part has
	// started (see isSequentialBlocked). Sourced from config
	// queue.sequential_parts (default true there; false here for tests).
	SequentialParts bool
	// AutoRetry is how many times run() re-queues an item after a transient
	// failure (see failTransient) before leaving it in StateError; 0
	// disables automatic retries. AutoRetryDelay is the first wait (default
	// 30s), doubled per attempt up to maxAutoRetryDelay.
	AutoRetry      int
	AutoRetryDelay time.Duration
	// Notifier, if non-nil, is told whenever a package finishes (see
	// checkFinished) — wired to internal/notify's webhook/ntfy delivery.
	Notifier PackageNotifier
	// AccountPriority, if non-nil, returns the current account priority
	// (plugin names, most preferred first — see RankHosters). Read on
	// every match so a change in the Web UI applies to queued items too.
	AccountPriority func() []string
}

// Manager owns all queue state (packages, items) and the worker pool that
// drives items through plugin resolution and the download engine. It is
// the single writer of queue state.
type Manager struct {
	store           *Store
	registry        *plugins.Registry
	engine          *downloader.Engine
	accounts        AccountLookup
	stats           StatsRecorder
	extractor       Extractor
	extractSettings ExtractSettings
	sched           *scheduler.Scheduler
	log             *slog.Logger
	cfg             Config

	// addMu serializes AddLinks' dedupe/group/merge decisions (see
	// grouping.go); it is never held together with mu for long.
	addMu sync.Mutex

	mu       sync.RWMutex
	items    map[string]*Item
	packages map[string]*Package
	posSeq   int64

	// extractTriggered de-duplicates extraction triggers for a given
	// package+archive-group (see maybeExtract): parallel downloads can
	// finish a multi-part archive's parts in any order, and more than
	// one completing around the same time would otherwise each see
	// "all parts done" and race to trigger extraction redundantly. Each
	// entry is removed once its runExtraction call completes (see that
	// function's defer), so a group that's since been deleted and
	// re-added can trigger extraction again rather than being
	// permanently stuck as "already triggered".
	extractTriggeredMu sync.Mutex
	extractTriggered   map[string]bool

	// diskCheckAttempts/diskCheckInterval are this Manager's effective
	// values for the package-level vars of the same purpose (see their
	// doc comment) — resolved once in New from Config.DiskCheckAttempts/
	// DiskCheckInterval, falling back to the package vars when unset.
	diskCheckAttempts int
	diskCheckInterval time.Duration

	// parallelFiles/activeFiles replace a fixed-capacity channel
	// semaphore so the concurrency limit can change live (see
	// SetParallelFiles, used by internal/settings). dispatch is called
	// concurrently from two different goroutines (the poll ticker and
	// the wake handler in Start), so admission is a CAS loop
	// (tryAdmit), not a plain load-then-increment — the latter would let
	// two concurrent dispatch calls both pass the limit check and both
	// admit, exceeding parallelFiles.
	parallelFiles atomic.Int64
	activeFiles   atomic.Int64
	// closing is set by Shutdown so dispatch stops admitting new downloads
	// while Shutdown waits for the in-flight ones to wind down.
	closingMu sync.Mutex
	closing   bool
	wake      chan struct{}

	runMu  sync.Mutex
	runCtx context.Context

	// active holds the live run of each item that has one. A run is
	// registered by pickNext together with the item's switch to
	// StateDownloading and removed by run() itself when it returns.
	activeMu  sync.Mutex
	active    map[string]*runHandle
	runningWG sync.WaitGroup

	// captchaSolutions holds a solution submitted via SolveCaptcha for an
	// item whose next run() attempt hasn't consumed it yet — see
	// withCaptchaSolution. Deliberately in-memory only, not persisted: a
	// captcha solution is single-use and short-lived (the retry it
	// triggers happens within moments), unlike CaptchaURL/RetryAfter on
	// Item itself, which do need to survive a restart to keep showing the
	// pending challenge/wait in the Web UI.
	captchaMu        sync.Mutex
	captchaSolutions map[string]string

	// finishedSig remembers, per package (or "item:<id>" for a package-less
	// item), the outcome checkFinished last notified about — see there.
	finishedMu  sync.Mutex
	finishedSig map[string]string

	subMu   sync.Mutex
	subs    map[int]chan Event
	nextSub int
}

// New builds a Manager and loads any persisted queue state from store. Any
// item found in StateDownloading (the daemon crashed or was killed
// mid-transfer) is reset to StateQueued; the downloader's own on-disk
// resume state picks up the transfer where it left off.
//
// extractor and extractSettings may both be nil, disabling post-download
// archive extraction entirely regardless of what ExtractSettings.
// ExtractEnabled would otherwise say — used by tests (and by any future
// caller) that don't need it wired up. stats may also be nil, disabling
// stats recording.
func New(store *Store, registry *plugins.Registry, engine *downloader.Engine, accounts AccountLookup, stats StatsRecorder, extractor Extractor, extractSettings ExtractSettings, sched *scheduler.Scheduler, log *slog.Logger, cfg Config) (*Manager, error) {
	if cfg.ParallelFiles < 1 {
		cfg.ParallelFiles = 3
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.AutoRetryDelay <= 0 {
		cfg.AutoRetryDelay = 30 * time.Second
	}
	if log == nil {
		return nil, fmt.Errorf("queue: logger is required")
	}
	diskCheckAtt := cfg.DiskCheckAttempts
	if diskCheckAtt <= 0 {
		diskCheckAtt = diskCheckAttempts
	}
	diskCheckIval := cfg.DiskCheckInterval
	if diskCheckIval <= 0 {
		diskCheckIval = diskCheckInterval
	}

	m := &Manager{
		store:             store,
		registry:          registry,
		engine:            engine,
		accounts:          accounts,
		stats:             stats,
		extractor:         extractor,
		extractSettings:   extractSettings,
		sched:             sched,
		log:               log,
		cfg:               cfg,
		items:             make(map[string]*Item),
		packages:          make(map[string]*Package),
		extractTriggered:  make(map[string]bool),
		diskCheckAttempts: diskCheckAtt,
		diskCheckInterval: diskCheckIval,
		wake:              make(chan struct{}, 1),
		active:            make(map[string]*runHandle),
		captchaSolutions:  make(map[string]string),
		finishedSig:       make(map[string]string),
		subs:              make(map[int]chan Event),
	}
	m.parallelFiles.Store(int64(cfg.ParallelFiles))

	items, err := store.ListItems()
	if err != nil {
		return nil, fmt.Errorf("queue: load items: %w", err)
	}
	for _, it := range items {
		if it.State == StateDownloading {
			it.State = StateQueued
			it.UpdatedAt = time.Now()
			_ = store.PutItem(it)
		}
		m.items[it.ID] = it
		if it.Position >= m.posSeq {
			m.posSeq = it.Position + 1
		}
	}
	pkgs, err := store.ListPackages()
	if err != nil {
		return nil, fmt.Errorf("queue: load packages: %w", err)
	}
	used := make(map[string]bool, len(pkgs))
	for _, it := range m.items {
		if it.PackageID != "" {
			used[it.PackageID] = true
		}
	}
	for _, p := range pkgs {
		// A package with no items left is an orphan from before empty
		// packages were cleaned up on Cancel (see removePackageIfEmpty) —
		// drop it instead of carrying it forever.
		if !used[p.ID] {
			if err := store.DeletePackage(p.ID); err != nil {
				log.Warn("queue: delete orphan package", "package_id", p.ID, "error", err)
			}
			continue
		}
		m.packages[p.ID] = p
	}

	return m, nil
}

// Start begins the dispatch loop: a periodic poll (Config.PollInterval)
// plus an immediate wake on every mutation. It also arms a one-shot timer
// for every loaded item with a future ScheduledAt, so scheduled items
// become runnable at (close to) their exact time rather than only at the
// next poll tick. Start returns immediately; dispatch runs in background
// goroutines until ctx is cancelled.
func (m *Manager) Start(ctx context.Context) {
	m.runMu.Lock()
	m.runCtx = ctx
	m.runMu.Unlock()

	m.resetInterruptedExtractions()

	m.sched.Every(ctx, m.cfg.PollInterval, func(ctx context.Context) { m.dispatch(ctx) })

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.wake:
				m.dispatch(ctx)
			}
		}
	}()

	m.mu.RLock()
	now := time.Now()
	for _, it := range m.items {
		if it.State != StateQueued {
			continue
		}
		if it.ScheduledAt != nil && it.ScheduledAt.After(now) {
			m.armSchedule(ctx, *it.ScheduledAt)
		}
		if it.RetryAfter != nil && it.RetryAfter.After(now) {
			m.armSchedule(ctx, *it.RetryAfter)
		}
	}
	m.mu.RUnlock()
}

// resetInterruptedExtractions recovers items left in
// ExtractStateExtracting by a prior hopperd crash or restart:
// runExtraction only ever runs while the process that started it is
// alive, so any item still showing "extracting" when a new Manager
// starts didn't finish — it just never got the chance to report success
// or failure. Left alone, it would sit in the Web UI showing
// "Extracting…" forever, since RetryExtraction only accepts an item
// already in ExtractStateExtractFailed. Called once, at startup, before
// the dispatcher (and therefore any real download or extraction) begins.
func (m *Manager) resetInterruptedExtractions() {
	const msg = "extraction interrupted by restart — use re-extract button"

	m.mu.Lock()
	var updated []*Item
	for _, it := range m.items {
		if it.ExtractState != ExtractStateExtracting {
			continue
		}
		it.ExtractState = ExtractStateExtractFailed
		it.ExtractError = msg
		it.UpdatedAt = time.Now()
		if err := m.store.PutItem(it); err != nil {
			m.log.Error("queue: persist item", "item_id", it.ID, "error", err)
		}
		cp := *it
		updated = append(updated, &cp)
	}
	m.mu.Unlock()

	for _, it := range updated {
		m.log.Warn("queue: extraction interrupted by restart, marked extract_failed", "item_id", it.ID)
		m.publish(Event{Type: EventItemUpdated, Item: it})
	}
}

// Shutdown cancels any in-flight downloads (returning them to StateQueued
// so they resume cleanly on next start, rather than StateError) and waits
// for their goroutines to exit, up to ctx's deadline.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.closingMu.Lock()
	m.closing = true
	m.closingMu.Unlock()
	m.activeMu.Lock()
	for _, h := range m.active {
		h.stop("shutdown")
	}
	m.activeMu.Unlock()

	done := make(chan struct{})
	go func() {
		m.runningWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stopping reports whether Shutdown has begun or the context Start was
// given is done.
func (m *Manager) stopping() bool {
	m.closingMu.Lock()
	closing := m.closing
	m.closingMu.Unlock()
	return closing || m.shutdownCtx().Err() != nil
}

func (m *Manager) armSchedule(ctx context.Context, at time.Time) {
	m.sched.At(ctx, at, func(ctx context.Context) { m.dispatch(ctx) })
}

// armWakeAt arms a one-shot dispatch wake at at using the Manager's own
// running context, the same mechanism Start/AddLink use for a future
// ScheduledAt (see armSchedule) — a no-op if Start hasn't been called
// yet. Used by tryFreeFallback so a plugins.ErrRetryAfter wait ends close
// to its exact deadline rather than only at the next PollInterval tick.
func (m *Manager) armWakeAt(at time.Time) {
	m.runMu.Lock()
	runCtx := m.runCtx
	m.runMu.Unlock()
	if runCtx != nil {
		m.armSchedule(runCtx, at)
	}
}

func (m *Manager) wakeAsync() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// dispatch acquires available worker slots and starts a goroutine for
// each runnable item it can find, until either parallelFiles is
// exhausted or no runnable item remains.
func (m *Manager) dispatch(ctx context.Context) {
	for {
		if ctx.Err() != nil || !m.tryAdmit() {
			return
		}
		// runningWG.Add happens under closingMu, before pickNext marks
		// anything as downloading, so Shutdown (which sets closing under
		// the same lock) can never start waiting while a new run is
		// still being admitted.
		m.closingMu.Lock()
		if m.closing {
			m.closingMu.Unlock()
			m.activeFiles.Add(-1)
			return
		}
		m.runningWG.Add(1)
		m.closingMu.Unlock()

		it, h := m.pickNext(ctx)
		if it == nil {
			m.runningWG.Done()
			m.activeFiles.Add(-1)
			return
		}
		go m.run(it, h)
	}
}

// tryAdmit atomically claims one worker slot if activeFiles is below the
// current parallelFiles limit. A CAS loop rather than a plain
// load-then-increment: dispatch runs concurrently from two goroutines
// (Start's poll ticker and its wake handler), and a non-atomic
// check-then-act would let both admit past the limit.
func (m *Manager) tryAdmit() bool {
	for {
		cur := m.activeFiles.Load()
		if cur >= m.parallelFiles.Load() {
			return false
		}
		if m.activeFiles.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// runHandle is one run of an item. The reason it was stopped on purpose
// (pause, cancel, shutdown) belongs to the run, not to the item ID: a run
// stuck in a plugin call that ignores its context can outlive a pause and
// resume, and must neither pick up nor clear the state of a later run.
type runHandle struct {
	ctx    context.Context
	cancel context.CancelFunc
	intent string // "pause"|"cancel"|"shutdown", or "" while running; guarded by Manager.activeMu
}

// stop records why the run is being stopped and cancels it. Must be called
// with Manager.activeMu held.
func (h *runHandle) stop(intent string) {
	h.intent = intent
	h.cancel()
}

// pickNext finds the first runnable item in queue order (see queueOrder —
// the same top-to-bottom order the Web UI shows), transitions it to
// StateDownloading under lock (so no other dispatch call can pick it up
// too) and registers its run, whose context derives from ctx. It returns
// a copy of the item and the run. With Config.SequentialParts, an archive
// part whose lower-numbered sibling hasn't started yet is skipped (see
// isSequentialBlocked). An item whose previous run hasn't returned yet is
// skipped too: it starts once that run is gone.
func (m *Manager) pickNext(ctx context.Context) (*Item, *runHandle) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Lock order: m.mu before m.activeMu, as everywhere else.
	m.activeMu.Lock()
	defer m.activeMu.Unlock()

	now := time.Now()
	order := newQueueOrder(m.items)
	var best *Item
	for _, it := range m.items {
		if !it.runnable(now) {
			continue
		}
		if _, running := m.active[it.ID]; running {
			continue
		}
		if m.cfg.SequentialParts && m.isSequentialBlocked(it) {
			continue
		}
		if best == nil || order.less(it, best) {
			best = it
		}
	}
	if best == nil {
		return nil, nil
	}
	best.State = StateDownloading
	best.Error = ""
	best.RetryAfter = nil
	best.CaptchaURL = ""
	best.UpdatedAt = now
	if err := m.store.PutItem(best); err != nil {
		m.log.Error("queue: persist item", "item_id", best.ID, "error", err)
	}
	rctx, cancel := context.WithCancel(ctx)
	h := &runHandle{ctx: rctx, cancel: cancel}
	m.active[best.ID] = h
	cp := *best
	m.publish(Event{Type: EventItemUpdated, Item: &cp})
	return &cp, h
}

// isSequentialBlocked reports whether it must not start yet because a
// lower-numbered part of the same archive set (same PackageID, same
// extract.GroupBase) hasn't started yet (still StateQueued) — parts never
// overtake each other. They may run side by side: once part1 has started,
// part2 can take the next free slot, so parallel_files fills the topmost
// package first instead of spreading over several packages.
//
// queueOrder already picks parts in part-number order; this guard covers
// the case where a lower part exists but isn't runnable right now (e.g.
// waiting on a hoster's retry_after), so a later part doesn't jump ahead.
// StateError and StatePaused don't block, or one dead or paused part would
// strand the rest of the set.
//
// Must be called with m.mu held.
func (m *Manager) isSequentialBlocked(it *Item) bool {
	if it.PackageID == "" || it.Filename == "" || !extract.IsArchive(it.Filename) {
		return false
	}
	n, ok := extract.PartNumber(it.Filename)
	if !ok {
		return false
	}
	group := extract.GroupBase(it.Filename)
	for _, sib := range m.items {
		if sib.ID == it.ID || sib.PackageID != it.PackageID || sib.Filename == "" {
			continue
		}
		if !extract.IsArchive(sib.Filename) || extract.GroupBase(sib.Filename) != group {
			continue
		}
		sn, sok := extract.PartNumber(sib.Filename)
		if !sok || sn >= n {
			continue
		}
		if sib.State == StateQueued {
			return true
		}
	}
	return false
}

// matchHoster resolves the plugin that should handle rawURL against this
// Manager's own registry/accounts. See MatchHoster for the actual
// two-pass priority logic.
//
// This is deliberately re-evaluated on every call (addItem at queue
// time, run at download time) rather than cached on the Item: adding or
// removing an account should retroactively change which plugin an
// already-queued link uses, without requiring the user to re-add it.
func (m *Manager) matchHoster(rawURL string) (plugins.Hoster, bool) {
	return MatchHoster(m.registry.All(), rawURL, m.accounts, m.priority())
}

// priority returns the current account priority, or nil (default order).
func (m *Manager) priority() []string {
	if m.cfg.AccountPriority == nil {
		return nil
	}
	return m.cfg.AccountPriority()
}

// matchHosterExcluding is matchHoster, but skips any plugin named in
// exclude. Used by run()'s retry loop to fall back to a different plugin
// after the one matchHoster would normally pick has just failed with
// plugins.ErrAccountRequired — most commonly a hoster account (Real-Debrid,
// Rapidgator) that's still configured (AccountFor still finds it) but has
// expired or been revoked, so Match() keeps claiming the URL even though
// Resolve/FetchDownloadURL can no longer actually use it. A plain re-call
// of matchHoster can't recover from this on its own: nothing about the
// registry or configured accounts actually changed between attempts, so
// it would deterministically pick the exact same plugin again — exclude
// is the only thing that changes between one call and the next.
func (m *Manager) matchHosterExcluding(rawURL string, exclude map[string]bool) (plugins.Hoster, bool) {
	all := m.registry.All()
	if len(exclude) == 0 {
		return MatchHoster(all, rawURL, m.accounts, m.priority())
	}
	filtered := make([]plugins.Hoster, 0, len(all))
	for _, h := range all {
		if !exclude[h.Name()] {
			filtered = append(filtered, h)
		}
	}
	return MatchHoster(filtered, rawURL, m.accounts, m.priority())
}

// MatchHoster picks the best plugin among all for rawURL using two-pass
// priority: pass 1 considers only plugins with a *configured account*
// (an account.Manager entry matching the plugin's Name()), in account
// priority order (see RankHosters) — so with accounts for several
// multi-hosters (Real-Debrid, AllDebrid, ...) and a native plugin, the
// one ranked highest that supports the URL wins. Pass 2 falls back to
// any matching plugin — accounts or not — for plugins like directhttp
// that have no account concept, or when no account-having plugin
// matches; there the last registered wins.
//
// Exported (not just Manager.matchHoster) so a Hoster plugin that needs
// to re-resolve a URL against the *whole* registry after its own
// preprocessing — currently just plugins/hidecx, unwrapping a
// hide.cx redirect so an already-configured Real-Debrid account can
// still claim the real hoster URL underneath — applies the identical
// priority rule instead of a second, potentially drifting copy of it.
// See cmd/hopperd/main.go's hidecx wiring.
func MatchHoster(all []plugins.Hoster, rawURL string, accounts AccountLookup, priority []string) (plugins.Hoster, bool) {
	if accounts != nil {
		for _, h := range RankHosters(all, priority) {
			if !h.Match(rawURL) {
				continue
			}
			if _, ok := accounts.AccountFor(h.Name()); ok {
				return h, true
			}
		}
	}

	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Match(rawURL) {
			return all[i], true
		}
	}
	return nil, false
}

// RankHosters orders all by account priority: plugins named in priority
// first, in that order (unknown names are ignored), then every other
// plugin with the last registered first — the default, which is why
// cmd/hopperd registers the multi-hosters after the native plugins and
// Real-Debrid last. The result is a new slice; all is not modified.
func RankHosters(all []plugins.Hoster, priority []string) []plugins.Hoster {
	out := make([]plugins.Hoster, 0, len(all))
	placed := make(map[string]bool, len(all))
	for _, name := range priority {
		if placed[name] {
			continue
		}
		for _, h := range all {
			if h.Name() == name {
				out = append(out, h)
				placed[name] = true
				break
			}
		}
	}
	for i := len(all) - 1; i >= 0; i-- {
		if !placed[all[i].Name()] {
			out = append(out, all[i])
		}
	}
	return out
}

func (m *Manager) run(it *Item, h *runHandle) {
	defer m.runningWG.Done()
	defer func() {
		m.activeFiles.Add(-1)
		m.wakeAsync()
	}()
	defer func() {
		h.cancel()
		m.activeMu.Lock()
		if m.active[it.ID] == h {
			delete(m.active, it.ID)
		}
		m.activeMu.Unlock()
	}()

	hoster, ok := m.matchHoster(it.URL)
	if !ok {
		m.finishWithIntentCheck(h, it.ID, func() { m.fail(it.ID, "no plugin registered for this URL") })
		return
	}

	// tried accumulates every plugin this run has already attempted and
	// had reject it with plugins.ErrAccountRequired — see
	// matchHosterExcluding's doc comment for why this loop can fall
	// through to e.g. a native Rapidgator plugin when a previously-working
	// Real-Debrid account has since expired, instead of failing forever
	// against the same now-unusable plugin on every retry.
	tried := map[string]bool{}
	// triedAccounts holds the IDs of accounts this run already had
	// rejected, so rotateAccount never cycles back to one of them.
	triedAccounts := map[string]bool{}

	var acc *plugins.Account
	var info *plugins.FileInfo
	var downloadURL string

	for {
		// Re-resolve rather than trust it.HosterKey: an account may have
		// been added or removed since the item was queued (matchHoster is
		// deliberately late-binding, see its doc comment), so the hoster
		// actually used here can differ from whatever was stored at add
		// time. Look up the account for *this* hoster, not the stale one,
		// and correct the stored key so the UI and per-hoster download
		// limits (downloader.Request.HosterKey below) reflect reality.
		if hoster.Name() != it.HosterKey {
			m.updateHosterKey(it.ID, hoster.Name())
			it.HosterKey = hoster.Name() // keep this goroutine's copy in sync too
		}

		acc = nil
		if m.accounts != nil {
			acc, _ = m.accounts.AccountFor(it.HosterKey)
		}

		var err error
		info, err = hoster.Resolve(it.URL, acc)
		if err != nil {
			if errors.Is(err, plugins.ErrAccountRequired) {
				if m.rotateAccount(it.HosterKey, acc, triedAccounts) {
					continue
				}
				tried[hoster.Name()] = true
				if next, found := m.matchHosterExcluding(it.URL, tried); found {
					hoster = next
					continue
				}
				m.finishWithIntentCheck(h, it.ID, func() {
					m.fail(it.ID, "no plugin with a configured account can handle this URL")
				})
				return
			}
			if errors.Is(err, plugins.ErrUnsupportedHost) {
				if next, found := m.nextPlugin(it, hoster, tried, err); found {
					hoster = next
					continue
				}
			}
			msg := fmt.Sprintf("resolve: %v", err)
			m.finishWithIntentCheck(h, it.ID, func() { m.failMaybeTransient(it.ID, msg, err) })
			return
		}
		if !info.Available {
			m.finishWithIntentCheck(h, it.ID, func() { m.fail(it.ID, "file unavailable at hoster") })
			return
		}

		downloadURL, err = hoster.FetchDownloadURL(it.URL, acc)
		if err != nil {
			if errors.Is(err, plugins.ErrAccountRequired) {
				if m.rotateAccount(it.HosterKey, acc, triedAccounts) {
					continue
				}
				tried[hoster.Name()] = true
				if next, found := m.matchHosterExcluding(it.URL, tried); found {
					hoster = next
					continue
				}
				// No other plugin can pick up this URL — before giving
				// up, see if hoster itself offers a free/limited fallback
				// (e.g. a free-tier account that can't use a premium-only
				// endpoint but can still download, subject to a wait or a
				// captcha). See tryFreeFallback's doc comment.
				url, ok := m.tryFreeFallback(h, it, hoster, acc)
				if !ok {
					return
				}
				downloadURL = url
			} else {
				if errors.Is(err, plugins.ErrUnsupportedHost) {
					if next, found := m.nextPlugin(it, hoster, tried, err); found {
						hoster = next
						continue
					}
				}
				msg := fmt.Sprintf("fetch download url: %v", err)
				m.finishWithIntentCheck(h, it.ID, func() { m.failMaybeTransient(it.ID, msg, err) })
				return
			}
		}
		break
	}

	req := downloader.Request{
		URL:          downloadURL,
		RelPath:      m.relPathFor(it, info),
		ExpectedSize: info.Size,
		HosterKey:    it.HosterKey,
		OnProgress: func(p downloader.ProgressUpdate) {
			m.updateProgress(it.ID, p)
		},
	}

	res, err := m.engine.Download(h.ctx, req)
	if err != nil {
		m.discardIfCancelled(h, it.ID, req.RelPath)
		// Always worth an automatic retry: it re-runs Resolve/
		// FetchDownloadURL (a fresh, unexpired direct link) and the
		// downloader resumes from the bytes already on disk.
		m.finishWithIntentCheck(h, it.ID, func() { m.failTransient(it.ID, err.Error()) })
		return
	}
	if res.Skipped {
		m.log.Info("queue: skipped: file already exists", "item_id", it.ID, "path", res.Path)
	}
	m.complete(it.ID, res)
}

// nextPlugin handles plugins.ErrUnsupportedHost from hoster: it marks
// hoster as tried for this run and returns the next matching plugin in
// account priority order, if any. Unlike ErrAccountRequired, the account
// isn't reported (no cooldown): it is fine, just not for this host now.
// With no plugin left, the caller fails the item as a transient error —
// the host may be back at the next automatic retry.
func (m *Manager) nextPlugin(it *Item, hoster plugins.Hoster, tried map[string]bool, cause error) (plugins.Hoster, bool) {
	tried[hoster.Name()] = true
	next, found := m.matchHosterExcluding(it.URL, tried)
	if found {
		m.log.Info("queue: plugin can't serve this host, trying next", "item_id", it.ID, "plugin", hoster.Name(), "next", next.Name(), "reason", cause)
	}
	return next, found
}

// rotateAccount handles a hoster rejecting acc (plugins.ErrAccountRequired
// — premium expired, traffic used up, credentials revoked) when the
// hoster has more than one account: it reports acc to the account store
// (AccountFailureReporter), which puts it into a short cooldown, and
// reports whether AccountFor now hands out an account this run hasn't
// tried yet. If so, run() retries the *same* plugin with that account
// before falling back to another plugin or the free-download path.
func (m *Manager) rotateAccount(hosterKey string, acc *plugins.Account, triedAccounts map[string]bool) bool {
	if acc == nil || acc.ID == "" {
		return false
	}
	reporter, ok := m.accounts.(AccountFailureReporter)
	if !ok {
		return false
	}
	triedAccounts[acc.ID] = true
	reporter.ReportFailure(acc.ID)
	next, ok := m.accounts.AccountFor(hosterKey)
	if !ok || next.ID == "" || triedAccounts[next.ID] {
		return false
	}
	m.log.Info("queue: account rejected, trying next account", "hoster", hosterKey)
	return true
}

// tryFreeFallback attempts hoster's plugins.FreeDownloader (if it
// implements one) as a last resort after every other plugin candidate for
// it.URL has already been exhausted (see run()'s FetchDownloadURL failure
// branch) — hoster's own normal Resolve already succeeded earlier in this
// attempt, only the premium/token download exchange failed with
// ErrAccountRequired, e.g. a free-tier account that can't use a
// premium-only endpoint.
//
// It fully owns every terminal outcome except success: scheduling a
// retry (ErrRetryAfter), recording a pending captcha challenge
// (ErrCaptchaRequired), or failing the item outright, all via the same
// finishWithIntentCheck-guarded path every other run() failure uses, so a
// concurrent pause/cancel is still respected here too. Success is
// reported back as (url, true) so run() can fall through to the normal
// download step using the *FileInfo already resolved earlier in this
// attempt; every other case returns ("", false), meaning "already fully
// handled — the caller should just return".
func (m *Manager) tryFreeFallback(h *runHandle, it *Item, hoster plugins.Hoster, acc *plugins.Account) (string, bool) {
	fd, ok := hoster.(plugins.FreeDownloader)
	if !ok {
		m.finishWithIntentCheck(h, it.ID, func() {
			m.fail(it.ID, "no plugin with a configured account can handle this URL")
		})
		return "", false
	}

	freeAcc := m.withCaptchaSolution(it.ID, acc)
	url, err := fd.FetchFreeDownloadURL(h.ctx, it.URL, freeAcc)
	if err == nil {
		return url, true
	}

	var retryErr plugins.ErrRetryAfter
	if errors.As(err, &retryErr) {
		at := time.Now().Add(time.Duration(retryErr.Seconds) * time.Second)
		m.finishWithIntentCheck(h, it.ID, func() { m.setRetryAfter(it.ID, at) })
		m.armWakeAt(at)
		return "", false
	}

	var captchaErr plugins.ErrCaptchaRequired
	if errors.As(err, &captchaErr) {
		m.finishWithIntentCheck(h, it.ID, func() { m.setCaptchaURL(it.ID, captchaErr.ImageURL) })
		return "", false
	}

	m.finishWithIntentCheck(h, it.ID, func() { m.fail(it.ID, fmt.Sprintf("free download: %v", err)) })
	return "", false
}

// withCaptchaSolution returns acc unchanged unless a solution was
// submitted for id via SolveCaptcha and not yet consumed, in which case
// it returns a copy of acc (or a fresh *Account if acc is nil) with
// Extra["captcha_solution"] set to it — see plugins.FreeDownloader's doc
// comment for that convention. The stored solution is consumed (deleted)
// here regardless of whether the plugin call that follows actually uses
// or accepts it: a wrong or stale solution shouldn't be silently retried
// forever against every subsequent attempt.
func (m *Manager) withCaptchaSolution(id string, acc *plugins.Account) *plugins.Account {
	m.captchaMu.Lock()
	solution, ok := m.captchaSolutions[id]
	if ok {
		delete(m.captchaSolutions, id)
	}
	m.captchaMu.Unlock()
	if !ok {
		return acc
	}

	out := plugins.Account{}
	if acc != nil {
		out = *acc
	}
	extra := make(map[string]string, len(out.Extra)+1)
	for k, v := range out.Extra {
		extra[k] = v
	}
	extra["captcha_solution"] = solution
	out.Extra = extra
	return &out
}

// setRetryAfter records that id's next attempt should wait until at
// (driven by a hoster's plugins.ErrRetryAfter — see tryFreeFallback)
// without treating this as a failure: the item stays StateQueued, just
// not runnable() until at passes (see Item.runnable). Clears CaptchaURL
// and any leftover Error, since only one of RetryAfter/CaptchaURL applies
// at a time and this isn't an error state.
func (m *Manager) setRetryAfter(id string, at time.Time) {
	m.mu.Lock()
	it, ok := m.items[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	atCopy := at
	it.State = StateQueued // pickNext already flipped it to StateDownloading on admission
	it.RetryAfter = &atCopy
	it.CaptchaURL = ""
	it.Error = ""
	it.UpdatedAt = time.Now()
	if err := m.store.PutItem(it); err != nil {
		m.log.Error("queue: persist item", "item_id", id, "error", err)
	}
	cp := *it
	m.mu.Unlock()

	m.publish(Event{Type: EventItemUpdated, Item: &cp})
}

// setCaptchaURL records that id needs a captcha solved before it can
// download further (see tryFreeFallback / plugins.ErrCaptchaRequired).
// The item stays StateQueued but not runnable() until SolveCaptcha clears
// CaptchaURL. Clears RetryAfter and any leftover Error for the same
// reason setRetryAfter clears CaptchaURL/Error.
func (m *Manager) setCaptchaURL(id, imageURL string) {
	m.mu.Lock()
	it, ok := m.items[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	it.State = StateQueued // pickNext already flipped it to StateDownloading on admission
	it.CaptchaURL = imageURL
	it.RetryAfter = nil
	it.Error = ""
	it.UpdatedAt = time.Now()
	if err := m.store.PutItem(it); err != nil {
		m.log.Error("queue: persist item", "item_id", id, "error", err)
	}
	cp := *it
	m.mu.Unlock()

	m.publish(Event{Type: EventItemUpdated, Item: &cp})
}

// ErrNoCaptchaPending is returned by SolveCaptcha when id has no
// outstanding captcha challenge (CaptchaURL is empty) to apply a solution
// to.
var ErrNoCaptchaPending = errors.New("queue: no captcha pending for this item")

// SolveCaptcha records solution for id's pending captcha challenge and
// clears CaptchaURL, making the item runnable() again immediately. The
// next dispatch pass re-runs run() from scratch — same as any other
// retry, re-resolving the hoster and re-attempting the normal premium
// flow first — and when it reaches the same FreeDownloader fallback
// again, withCaptchaSolution hands the plugin this solution via
// acc.Extra["captcha_solution"] instead of failing again with
// ErrCaptchaRequired.
func (m *Manager) SolveCaptcha(id, solution string) error {
	m.mu.Lock()
	it, ok := m.items[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	if it.CaptchaURL == "" {
		m.mu.Unlock()
		return ErrNoCaptchaPending
	}
	it.CaptchaURL = ""
	it.UpdatedAt = time.Now()
	if err := m.store.PutItem(it); err != nil {
		m.log.Error("queue: persist item", "item_id", id, "error", err)
	}
	cp := *it
	m.mu.Unlock()

	m.captchaMu.Lock()
	m.captchaSolutions[id] = solution
	m.captchaMu.Unlock()

	m.publish(Event{Type: EventItemUpdated, Item: &cp})
	m.wakeAsync()
	return nil
}

// finishWithIntentCheck consults the pause/cancel/shutdown intent recorded
// for run h of item id and only invokes onError if the failure wasn't
// caused by one of those intentional actions, which already applied their
// own terminal state.
//
// A run that ends while the Manager is stopping counts as shut down even
// without a recorded intent: hopperd's signal context, which Start
// received, is cancelled before Shutdown gets to record one, and the
// "context canceled" that follows must not use up an automatic retry.
func (m *Manager) finishWithIntentCheck(h *runHandle, id string, onError func()) {
	m.activeMu.Lock()
	intent := h.intent
	m.activeMu.Unlock()
	if intent == "" && m.stopping() {
		intent = "shutdown"
	}

	switch intent {
	case "pause", "cancel":
		return
	case "shutdown":
		m.mu.Lock()
		if it, ok := m.items[id]; ok {
			it.State = StateQueued
			it.UpdatedAt = time.Now()
			_ = m.store.PutItem(it)
		}
		m.mu.Unlock()
		return
	default:
		onError()
	}
}

func (m *Manager) relPathFor(it *Item, info *plugins.FileInfo) string {
	name := it.Filename
	if name == "" {
		name = info.Name
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.joinPackageDir(it.PackageID, name)
}

// packageDir returns the subdirectory items in pkg download into: an
// explicit "save to" override (pkg.Path — see SetPackagePath) if set,
// else pkg.Name as before. This is cosmetic organization only, not the
// security boundary: the downloader resolves the full RelPath via
// pathsafe.SafeJoin against BaseDir regardless, which is what actually
// rejects traversal — and is also why, unlike pkg.Name, pkg.Path isn't
// run through SanitizeFilename here: Path may legitimately be
// multi-segment (e.g. "movies/2026"), which SanitizeFilename's
// single-path-element rules would collapse to just "2026".
func packageDir(pkg *Package) string {
	if dir := strings.Trim(strings.ReplaceAll(pkg.Path, "\\", "/"), "/"); dir != "" {
		return dir
	}
	if pkg.Name == "" {
		return ""
	}
	return pathsafe.SanitizeFilename(pkg.Name)
}

func (m *Manager) updateProgress(id string, p downloader.ProgressUpdate) {
	m.mu.Lock()
	it, ok := m.items[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	it.Downloaded = p.BytesDownloaded
	if p.TotalBytes > 0 {
		it.Size = p.TotalBytes
	}
	it.Speed = p.Speed
	it.UpdatedAt = time.Now()
	cp := *it
	m.mu.Unlock()

	m.publish(Event{Type: EventItemUpdated, Item: &cp})
}

// updateHosterKey persists a corrected HosterKey for id (see run()'s late
// re-resolution via matchHoster) without touching state/progress. Persist
// errors are logged, not returned: this is a best-effort correction of
// display/limit metadata, not something worth failing the download over.
func (m *Manager) updateHosterKey(id, hosterKey string) {
	m.mu.Lock()
	it, ok := m.items[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	it.HosterKey = hosterKey
	it.UpdatedAt = time.Now()
	if err := m.store.PutItem(it); err != nil {
		m.log.Error("queue: persist item", "item_id", id, "error", err)
	}
	cp := *it
	m.mu.Unlock()

	m.publish(Event{Type: EventItemUpdated, Item: &cp})
}

func (m *Manager) fail(id, msg string) {
	// Stats are recorded before the state change becomes observable (via
	// Get/List or the published event), not after: a caller that waits for
	// StateError and then reads the stats would otherwise race the
	// RecordError call. The hoster key is read under a short read lock so
	// the (bbolt-backed, fsyncing) stats write never runs under m.mu.
	if m.stats != nil {
		if hk, ok := m.hosterKeyOf(id); ok {
			m.stats.RecordError(hk)
		}
	}

	m.mu.Lock()
	it, ok := m.items[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	it.State = StateError
	it.Error = msg
	it.UpdatedAt = time.Now()
	if err := m.store.PutItem(it); err != nil {
		m.log.Error("queue: persist item", "item_id", id, "error", err)
	}
	cp := *it
	m.mu.Unlock()

	m.log.Warn("queue: item failed", "item_id", id, "error", msg)
	m.publish(Event{Type: EventItemUpdated, Item: &cp})
	m.checkFinished(id)
}

// maxAutoRetryDelay caps the exponential automatic-retry backoff.
const maxAutoRetryDelay = 30 * time.Minute

// failMaybeTransient is fail() for errors that may or may not be worth an
// automatic retry: a definite "file not found" is permanent, anything
// else coming back from a hoster plugin (temporary unavailability, rate
// limiting, network errors talking to its API) goes through failTransient.
func (m *Manager) failMaybeTransient(id, msg string, err error) {
	if errors.Is(err, plugins.ErrNotFound) {
		m.fail(id, msg)
		return
	}
	m.failTransient(id, msg)
}

// failTransient re-queues id with an exponential RetryAfter delay instead
// of failing it, until Config.AutoRetry attempts are used up — then it
// falls through to a normal, final fail(). The item stays StateQueued
// (not runnable until RetryAfter, see Item.runnable), so the Web UI shows
// the existing "waiting" countdown; Error carries the reason meanwhile.
// Stats only count the final failure.
func (m *Manager) failTransient(id, msg string) {
	m.mu.Lock()
	it, ok := m.items[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	if m.cfg.AutoRetry <= 0 || it.Attempts >= m.cfg.AutoRetry {
		m.mu.Unlock()
		m.fail(id, msg)
		return
	}
	it.Attempts++
	delay := m.cfg.AutoRetryDelay << (it.Attempts - 1)
	if delay <= 0 || delay > maxAutoRetryDelay {
		delay = maxAutoRetryDelay
	}
	at := time.Now().Add(delay)
	it.State = StateQueued
	it.RetryAfter = &at
	it.Speed = 0
	it.Error = fmt.Sprintf("%s (auto-retry %d/%d)", msg, it.Attempts, m.cfg.AutoRetry)
	it.UpdatedAt = time.Now()
	if err := m.store.PutItem(it); err != nil {
		m.log.Error("queue: persist item", "item_id", id, "error", err)
	}
	cp := *it
	m.mu.Unlock()

	m.log.Warn("queue: transient failure, retrying later", "item_id", id, "attempt", cp.Attempts, "retry_in", delay, "error", msg)
	m.publish(Event{Type: EventItemUpdated, Item: &cp})
	m.armWakeAt(at)
}

// hosterKeyOf returns id's current HosterKey under a short read lock.
func (m *Manager) hosterKeyOf(id string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	it, ok := m.items[id]
	if !ok {
		return "", false
	}
	return it.HosterKey, true
}

func (m *Manager) complete(id string, res *downloader.Result) {
	// Same ordering as fail(): record stats before StateDone is observable.
	if m.stats != nil {
		if hk, ok := m.hosterKeyOf(id); ok {
			m.stats.RecordDownload(hk, res.BytesWritten, res.PeakSpeed)
		}
	}

	m.mu.Lock()
	it, ok := m.items[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	it.State = StateDone
	it.Attempts = 0
	if it.Filename == "" {
		// Never backfilled elsewhere: an item added without an explicit
		// filename only ever had the hoster-resolved name live in the
		// downloader's own Request/Result, not persisted on the Item
		// itself. Needed both to show a real filename in the UI/history
		// after completion (instead of the raw URL forever) and for
		// maybeExtract below, which has to recognize archive filenames.
		it.Filename = filepath.Base(res.Path)
	}
	it.Downloaded = res.BytesWritten
	it.Size = res.BytesWritten
	it.Speed = 0
	it.Error = ""
	it.UpdatedAt = time.Now()
	if err := m.store.PutItem(it); err != nil {
		m.log.Error("queue: persist item", "item_id", id, "error", err)
	}
	cp := *it
	m.mu.Unlock()

	m.publish(Event{Type: EventItemUpdated, Item: &cp})

	m.maybeExtract(&cp)
	m.checkFinished(id)
}

// maybeExtract checks whether a just-completed item's file — or, for a
// multi-part RAR, the whole archive group it belongs to — is ready to
// extract, and if so runs extraction in the background. "Ready" for a
// multi-part group means every sibling item in the same package whose
// filename maps to the same extract.GroupBase is StateDone, checked
// fresh on every completion (not just the first part's): with
// parallelFiles > 1, parts can finish in any order, so the part that
// happens to finish *last* is whichever one actually observes the group
// as complete.
func (m *Manager) maybeExtract(it *Item) {
	if m.extractor == nil || m.extractSettings == nil {
		return
	}
	if it.Filename == "" || !extract.IsArchive(it.Filename) {
		return
	}
	// Every reason not to extract is logged: otherwise a finished archive
	// that stays packed can't be told apart from a bug.
	if !m.extractSettings.ExtractEnabled() {
		m.log.Info("queue: auto-extract is off, archive not extracted", "item_id", it.ID, "archive", it.Filename)
		return
	}
	group := extract.GroupBase(it.Filename)

	m.mu.RLock()
	var firstPart *Item
	var siblings []*Item
	pending, stuck := 0, 0
	for _, sib := range m.items {
		if sib.PackageID != it.PackageID || sib.Filename == "" {
			continue
		}
		if !extract.IsArchive(sib.Filename) || extract.GroupBase(sib.Filename) != group {
			continue
		}
		if sib.State != StateDone {
			pending++
			// Paused or failed parts never finish without the user,
			// unlike queued or downloading ones.
			if sib.State == StatePaused || sib.State == StateError {
				stuck++
			}
		}
		cp := *sib
		siblings = append(siblings, &cp)
		if extract.IsFirstPart(sib.Filename) {
			fp := cp
			firstPart = &fp
		}
	}
	pkg := m.packages[it.PackageID]
	m.mu.RUnlock()

	switch {
	case stuck > 0:
		m.log.Info("queue: extract waits for parts that won't finish on their own (paused or failed); start or retry them",
			"item_id", it.ID, "archive", group, "waiting_for", stuck, "pending", pending)
		return
	case pending > 0:
		m.log.Debug("queue: extract waits for remaining parts", "item_id", it.ID, "archive", group, "pending", pending)
		return
	case firstPart == nil:
		m.log.Warn("queue: extract: first part of the archive is not in this package, nothing extracted",
			"item_id", it.ID, "archive", group)
		return
	}

	triggerKey := it.PackageID + "|" + group
	m.extractTriggeredMu.Lock()
	if m.extractTriggered[triggerKey] {
		m.extractTriggeredMu.Unlock()
		return
	}
	m.extractTriggered[triggerKey] = true
	m.extractTriggeredMu.Unlock()

	pkgPassword := ""
	if pkg != nil {
		pkgPassword = pkg.Password
	}
	go m.runExtraction(firstPart, siblings, group, pkgPassword, "")
}

// diskCheckAttempts/diskCheckInterval are the package-level defaults for
// how long runExtraction waits for every sibling's file to actually be
// visible on disk before giving up — every sibling item already reports
// StateDone at this point (maybeExtract's own gate), but a download
// completing and its file being fully durable/visible aren't perfectly
// simultaneous (a network-mounted /downloads volume under Docker is the
// realistic case, not a bug in the downloader itself). 10 attempts *
// 500ms = up to 5s — raised from the original 5*200ms=1s default, which
// proved too short for slower NAS mounts in practice. Overridable per
// Manager via Config.DiskCheckAttempts/DiskCheckInterval (see New),
// sourced from config.ExtractConfig's part_wait_attempts/
// part_wait_interval — these package vars are now only the fallback
// when a Config doesn't set them.
// var, not const, so tests can shrink them for fast, deterministic
// coverage of the "still missing after every retry" path.
var (
	diskCheckAttempts = 10
	diskCheckInterval = 500 * time.Millisecond
)

// extractProgressInterval controls how often runExtraction logs that an
// in-progress extraction is still running (see runExtraction) — without
// this, a large multi-part RAR set taking many minutes produces no log
// output at all between "extracting" and either its success or failure,
// which reads as hung rather than working.
const extractProgressInterval = 60 * time.Second

// allPartsOnDisk verifies every sibling's resolved file actually exists,
// rather than trusting in-memory Item.State alone — see diskCheckAttempts.
func (m *Manager) allPartsOnDisk(siblings []*Item) (bool, error) {
	for _, sib := range siblings {
		path, err := pathsafe.SafeJoin(m.engine.BaseDir(), m.relPathFor(sib, nil))
		if err != nil {
			return false, err
		}
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

// runExtraction extracts firstPart's archive (unrar locates the
// remaining parts of a multi-part set itself) and, if
// ExtractSettings.ExtractDeleteAfter says so, deletes every part on
// success. Runs in its own goroutine (see maybeExtract) so a slow
// extraction doesn't hold up the dispatcher or the completion event.
//
// extraPassword, if non-empty, is tried ahead of the persisted
// ExtractPasswords() list for this call only — used by RetryExtraction
// to let a user supply a one-off password without writing it to
// settings; the automatic maybeExtract path always passes "".
func (m *Manager) runExtraction(firstPart *Item, siblings []*Item, group, pkgPassword, extraPassword string) {
	start := time.Now()

	// Releases the group's claim (set by maybeExtract or RetryExtraction)
	// regardless of how this call ends: without this,
	// deleting every item in an archive group and re-adding the same
	// archive later would never trigger extraction again, since
	// extractTriggered has no other eviction path.
	triggerKey := firstPart.PackageID + "|" + group
	// Registered before the dedup cleanup below, so it runs *after* it
	// (defers are LIFO): checkFinished must no longer see this group as
	// pending.
	defer m.checkFinished(firstPart.ID)
	defer func() {
		m.extractTriggeredMu.Lock()
		delete(m.extractTriggered, triggerKey)
		m.extractTriggeredMu.Unlock()
	}()

	relPath := m.relPathFor(firstPart, nil)
	srcPath, err := pathsafe.SafeJoin(m.engine.BaseDir(), relPath)
	if err != nil {
		m.log.Error("queue: extract: resolve archive path", "item_id", firstPart.ID, "error", err)
		m.setExtractState(firstPart.PackageID, group, ExtractStateExtractFailed, err.Error())
		return
	}
	destDir := filepath.Dir(srcPath)

	ready := false
	for attempt := 0; attempt < m.diskCheckAttempts; attempt++ {
		ok, err := m.allPartsOnDisk(siblings)
		if err != nil {
			m.log.Error("queue: extract: check parts on disk", "item_id", firstPart.ID, "error", err)
			m.setExtractState(firstPart.PackageID, group, ExtractStateExtractFailed, err.Error())
			return
		}
		if ok {
			ready = true
			break
		}
		if attempt < m.diskCheckAttempts-1 {
			time.Sleep(m.diskCheckInterval)
		}
	}
	if !ready {
		msg := "not all archive parts are present on disk"
		m.log.Warn("queue: extract: "+msg, "item_id", firstPart.ID, "archive", srcPath, "group", group)
		m.setExtractState(firstPart.PackageID, group, ExtractStateExtractFailed, msg)
		return
	}

	m.setExtractState(firstPart.PackageID, group, ExtractStateExtracting, "")

	// Bound to the Manager's own shutdown context (not
	// context.Background()): if hopperd shuts down mid-extraction, the
	// spawned unrar process should be killed along with everything else
	// in flight, not orphaned to keep running past the process that
	// started it. Still capped at extractTimeout either way.
	ctx, cancel := context.WithTimeout(m.shutdownCtx(), extractTimeout)
	defer cancel()

	// Logs "still running" every extractProgressInterval for the
	// duration of the actual extractor.Extract call below — a large
	// multi-part RAR set can legitimately take many minutes, and with no
	// progress output in between "extracting" and the final result, that
	// looks indistinguishable from hung.
	progressDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(extractProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-ticker.C:
				m.log.Info("queue: extract still running", "item_id", firstPart.ID, "archive", srcPath, "elapsed", time.Since(start).Round(time.Second))
			}
		}
	}()

	userPasswords := m.extractSettings.ExtractPasswords()
	if extraPassword != "" {
		userPasswords = append([]string{extraPassword}, userPasswords...)
	}

	res, err := m.extractor.Extract(ctx, srcPath, destDir, pkgPassword, userPasswords)
	close(progressDone)
	elapsed := time.Since(start).Round(time.Second)

	if err != nil {
		m.log.Warn("queue: extract failed", "item_id", firstPart.ID, "archive", srcPath, "elapsed", elapsed, "error", err)
		m.setExtractState(firstPart.PackageID, group, ExtractStateExtractFailed, err.Error())
		return
	}
	m.log.Info("queue: extracted archive", "item_id", firstPart.ID, "archive", srcPath, "files", len(res.ExtractedFiles), "elapsed", elapsed)
	m.setExtractState(firstPart.PackageID, group, ExtractStateExtracted, "")

	if m.extractSettings.ExtractDeleteAfter() {
		m.deleteArchiveParts(firstPart.PackageID, group)
	}
}

// shutdownCtx returns the context Start() was called with (m.runCtx),
// or context.Background() if Start hasn't run yet — the latter only
// happens in tests that construct a Manager but never call Start before
// exercising runExtraction/RetryExtraction directly.
func (m *Manager) shutdownCtx() context.Context {
	m.runMu.Lock()
	ctx := m.runCtx
	m.runMu.Unlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// setExtractState updates ExtractState/ExtractError on every sibling item
// in packageID+group — extraction is triggered once per group (see
// maybeExtract) and its outcome applies to every part of that group, not
// just the one item that happened to trigger it — persists each, and
// publishes an update per item so the Web UI's per-row status reflects
// it live over the WebSocket.
func (m *Manager) setExtractState(packageID, group string, state ExtractState, errMsg string) {
	m.mu.Lock()
	var updated []*Item
	for _, sib := range m.items {
		if sib.PackageID != packageID || sib.Filename == "" {
			continue
		}
		if !extract.IsArchive(sib.Filename) || extract.GroupBase(sib.Filename) != group {
			continue
		}
		sib.ExtractState = state
		sib.ExtractError = errMsg
		sib.UpdatedAt = time.Now()
		if err := m.store.PutItem(sib); err != nil {
			m.log.Error("queue: persist item", "item_id", sib.ID, "error", err)
		}
		cp := *sib
		updated = append(updated, &cp)
	}
	m.mu.Unlock()

	for _, it := range updated {
		m.publish(Event{Type: EventItemUpdated, Item: it})
	}
}

// deleteArchiveParts removes every on-disk file belonging to a
// successfully-extracted archive group (all parts of a multi-part RAR
// set, or the single zip/tar.{gz,bz2} file).
func (m *Manager) deleteArchiveParts(packageID, group string) {
	m.mu.RLock()
	var siblings []*Item
	for _, sib := range m.items {
		if sib.PackageID == packageID && sib.Filename != "" &&
			extract.IsArchive(sib.Filename) && extract.GroupBase(sib.Filename) == group {
			cp := *sib
			siblings = append(siblings, &cp)
		}
	}
	m.mu.RUnlock()

	for _, sib := range siblings {
		path, err := pathsafe.SafeJoin(m.engine.BaseDir(), m.relPathFor(sib, nil))
		if err != nil {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			m.log.Warn("queue: delete archive part after extract", "path", path, "error", err)
		}
	}
}

// ErrExtractNotRetryable is returned by RetryExtraction when the item
// isn't a finished archive waiting for extraction: it must be StateDone
// and either never extracted (auto-extract was off when it finished) or
// with a failed extraction. One still downloading, being extracted or
// already extracted is rejected.
var ErrExtractNotRetryable = errors.New("queue: item is not a finished archive that is unextracted or failed to extract")

// ErrArchiveIncomplete is returned by RetryExtraction when another part
// of the item's multi-part archive isn't done yet (e.g. still paused):
// extraction would only fail on the missing volume.
var ErrArchiveIncomplete = errors.New("queue: not every part of the archive is downloaded yet")

// ErrExtractInProgress is returned by RetryExtraction when the item's
// archive is already being extracted (another request, or the automatic
// extraction after the last part finished).
var ErrExtractInProgress = errors.New("queue: archive is already being extracted")

// ErrArchiveMissing is returned by RetryExtraction when the item's
// archive (or, for a multi-part set, its first part) can no longer be
// found on disk — e.g. deleted manually, or by a prior successful
// extraction's ExtractDeleteAfter cleanup somehow followed by a stale
// extract_failed record.
var ErrArchiveMissing = errors.New("queue: archive file not found on disk")

// RetryExtraction extracts id's archive on request: id must be StateDone
// and either never extracted (auto-extract was off when it finished) or
// with a failed extraction (ErrExtractNotRetryable otherwise), every part
// of its archive set must be done (ErrArchiveIncomplete otherwise), and
// its first part must still be on disk (ErrArchiveMissing otherwise).
// It works whether auto-extract is on or not. extraPassword, if non-empty, is tried
// ahead of the persisted extract_passwords list for this attempt only —
// see runExtraction's doc comment; it is never written to settings.
//
// Every sibling in the archive group has its ExtractState/ExtractError
// cleared (reset to "") before extraction is kicked off in the
// background, mirroring the "no extract state yet" starting point a
// fresh completion event would see — setExtractState (called from
// runExtraction almost immediately after) then republishes
// ExtractStateExtracting per sibling over the same EventItemUpdated
// path the Web UI already listens on.
func (m *Manager) RetryExtraction(id, extraPassword string) error {
	m.mu.RLock()
	it, ok := m.items[id]
	if !ok {
		m.mu.RUnlock()
		return ErrNotFound
	}
	if it.State != StateDone || (it.ExtractState != "" && it.ExtractState != ExtractStateExtractFailed) {
		m.mu.RUnlock()
		return ErrExtractNotRetryable
	}
	if it.Filename == "" || !extract.IsArchive(it.Filename) {
		m.mu.RUnlock()
		return ErrExtractNotRetryable
	}
	group := extract.GroupBase(it.Filename)
	pkgID := it.PackageID

	var firstPart *Item
	var siblings []*Item
	allDone := true
	for _, sib := range m.items {
		if sib.PackageID != it.PackageID || sib.Filename == "" {
			continue
		}
		if !extract.IsArchive(sib.Filename) || extract.GroupBase(sib.Filename) != group {
			continue
		}
		if sib.State != StateDone {
			allDone = false
		}
		cp := *sib
		siblings = append(siblings, &cp)
		if extract.IsFirstPart(sib.Filename) {
			fp := cp
			firstPart = &fp
		}
	}
	pkg := m.packages[it.PackageID]
	m.mu.RUnlock()

	if !allDone {
		return ErrArchiveIncomplete
	}
	if firstPart == nil {
		return ErrArchiveMissing
	}

	srcPath, err := pathsafe.SafeJoin(m.engine.BaseDir(), m.relPathFor(firstPart, nil))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrArchiveMissing, err)
	}
	if _, err := os.Stat(srcPath); err != nil {
		if os.IsNotExist(err) {
			return ErrArchiveMissing
		}
		return fmt.Errorf("%w: %w", ErrArchiveMissing, err)
	}

	if m.extractor == nil || m.extractSettings == nil {
		return fmt.Errorf("queue: extraction is not configured")
	}

	pkgPassword := ""
	if pkg != nil {
		pkgPassword = pkg.Password
	}

	// Claim the group the same way maybeExtract does (runExtraction
	// releases it), so concurrent requests — or a request while the
	// automatic extraction runs — start one extraction, not two writing
	// into the same directory. The state is checked again once claimed: a
	// request that read it before another extraction finished must not
	// extract the archive a second time.
	triggerKey := pkgID + "|" + group
	m.extractTriggeredMu.Lock()
	if m.extractTriggered[triggerKey] {
		m.extractTriggeredMu.Unlock()
		return ErrExtractInProgress
	}
	m.extractTriggered[triggerKey] = true
	m.extractTriggeredMu.Unlock()

	m.mu.RLock()
	cur, ok := m.items[id]
	stillPending := ok && (cur.ExtractState == "" || cur.ExtractState == ExtractStateExtractFailed)
	m.mu.RUnlock()
	if !stillPending {
		m.extractTriggeredMu.Lock()
		delete(m.extractTriggered, triggerKey)
		m.extractTriggeredMu.Unlock()
		return ErrExtractNotRetryable
	}

	m.setExtractState(pkgID, group, "", "")
	go m.runExtraction(firstPart, siblings, group, pkgPassword, extraPassword)
	return nil
}

// AddOptions customizes a newly added Item.
type AddOptions struct {
	Priority    Priority
	ScheduledAt *time.Time
	// HosterKey overrides plugin auto-detection when set.
	HosterKey string
	// Filename overrides the hoster-reported filename when set.
	Filename string
	// Password is an optional hoster/archive password (e.g. from a DLC
	// container or Click'n'Load submission), passed through to the Item
	// (and the Package, for AddPackage) for plugins that need it.
	Password string
	// StartPaused makes the item start in StatePaused instead of
	// StateQueued, so it won't be picked up by the dispatcher until the
	// user explicitly resumes it. Used by the DLC import and
	// Click'n'Load paths when config.Downloads.AutoStartImports is
	// false (the default) — a batch import shouldn't immediately
	// saturate the download slots with links the user hasn't reviewed.
	// Links added via AddLink directly (POST /api/v1/links) don't set
	// this.
	StartPaused bool
	// Size is a known file size to show before any download activity —
	// e.g. from a plugins.ExpandedLink's own Size, discovered while
	// expanding a container (see addExpandedItems). 0 (the common case,
	// and what every existing caller leaves it as) means unknown; the
	// item's real Size is still set from the hoster's own report once
	// the download actually starts (see updateProgress/complete), same
	// as before this field existed.
	Size int64
	// Filenames provides a per-URL filename for AddPackage, parallel to
	// its urls slice (Filenames[i] is the real filename for urls[i]) —
	// used when the caller already knows every file's real name up
	// front, e.g. a DLC container's own per-file "filename" field
	// (always present in DLC XML — see internal/dlc.File), instead of
	// leaving it to be discovered later via the matched hoster plugin's
	// Resolve call. Ignored by AddLink, which has only one URL and uses
	// the singular Filename field above instead. A shorter Filenames
	// slice, or an empty string at a given index, leaves that URL's
	// filename unset — same "discovered later" behavior AddPackage
	// always had before this field existed.
	//
	// Setting this up front matters beyond cosmetics: it's what lets
	// isSequentialBlocked (see Config.SequentialParts) recognize a
	// multi-part archive group at all. An item whose Filename is still
	// empty at add time can't be recognized as part of one until its
	// actual download starts, by which point there's nothing left for
	// sequential ordering to decide.
	Filenames []string
}

// AddLink enqueues a single URL, not belonging to any package.
func (m *Manager) AddLink(rawURL string, opts AddOptions) (*Item, error) {
	return m.addItem("", rawURL, opts)
}

// AddPackage creates a Package and enqueues one Item per URL under it.
// Invalid URLs are skipped; the returned error is non-nil only if every
// URL was invalid.
func (m *Manager) AddPackage(name string, urls []string, opts AddOptions) (*Package, []*Item, error) {
	pkg := &Package{ID: newID(), Name: name, Password: opts.Password, CreatedAt: time.Now()}
	if err := m.store.PutPackage(pkg); err != nil {
		return nil, nil, fmt.Errorf("queue: persist package: %w", err)
	}
	m.mu.Lock()
	m.packages[pkg.ID] = pkg
	m.mu.Unlock()
	pkgCopy := *pkg
	m.publish(Event{Type: EventPackageUpdated, Package: &pkgCopy})

	items := make([]*Item, 0, len(urls))
	var firstErr error
	for i, u := range urls {
		itemOpts := opts
		if i < len(opts.Filenames) {
			itemOpts.Filename = opts.Filenames[i]
		}
		added, err := m.addItems(pkg.ID, u, itemOpts)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		items = append(items, added...)
	}
	if len(items) == 0 {
		m.removePackageIfEmpty(pkg.ID)
		return &pkgCopy, nil, fmt.Errorf("queue: no valid URLs in package: %w", firstErr)
	}
	m.mu.RLock()
	pkgCopy = *pkg
	m.mu.RUnlock()
	return &pkgCopy, items, nil
}

// addItem enqueues a single rawURL and returns the one Item it produced.
// A thin wrapper over addItems for callers (AddLink, and every existing
// direct test call) that only ever expect one Item back — even when
// rawURL expands into several (see addItems), only the first is
// returned here; AddPackage calls addItems directly instead, so it can
// report every expanded item, not just one per input URL.
func (m *Manager) addItem(pkgID, rawURL string, opts AddOptions) (*Item, error) {
	items, err := m.addItems(pkgID, rawURL, opts)
	if err != nil {
		return nil, err
	}
	return items[0], nil
}

// addItems enqueues rawURL, first checking whether the plugin that would
// handle it actually represents *multiple* downloadable files behind
// one URL (plugins.Expander — see plugins/hidecx, whose hide.cx
// containers can hold every part of a multi-part archive). If so, all
// of them are added as one package via addExpandedItems instead of a
// single item for rawURL itself. An explicit opts.HosterKey bypasses
// this (as it already bypasses matchHoster below) — an explicit
// override means the caller wants direct control, not auto-expansion.
func (m *Manager) addItems(pkgID, rawURL string, opts AddOptions) ([]*Item, error) {
	if err := validHTTPURL(rawURL); err != nil {
		return nil, err
	}

	if opts.HosterKey == "" {
		if hoster, ok := m.matchHoster(rawURL); ok {
			if exp, ok := hoster.(plugins.Expander); ok {
				if links, expErr := exp.Expand(rawURL); expErr == nil && len(links) > 0 {
					return m.addExpandedItems(pkgID, links, opts)
				}
				// Expansion not usable right now (transient API error, or
				// the plugin found nothing to expand) — fall through and
				// queue rawURL as a single item. The plugin's own
				// Resolve/FetchDownloadURL retry the same lookup at run
				// time, so this URL isn't lost, just not expanded yet.
			}
		}
	}

	it, err := m.addPlainItem(pkgID, rawURL, opts)
	if err != nil {
		return nil, err
	}
	return []*Item{it}, nil
}

func validHTTPURL(rawURL string) error {
	u, err := url.ParseRequestURI(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("queue: invalid URL %q", rawURL)
	}
	return nil
}

// addExpandedItems inserts one Item per link, all under one package —
// a new one if pkgID is empty (this call came from AddLink rather than
// AddPackage), otherwise the existing one AddPackage's caller already
// created. The package's password is set from the links if any of them
// reported one and the package doesn't already have one from opts —
// see plugins.ExpandedLink.Password. Each item goes through
// addPlainItem with its *own* real hoster URL (not the original
// container URL), so matchHoster runs again against that URL — this is
// what lets an already-configured Real-Debrid account claim a
// hide.cx-resolved rapidgator.net link exactly like any other
// rapidgator.net link, per plugins/hidecx's Expand doc comment.
func (m *Manager) addExpandedItems(pkgID string, links []plugins.ExpandedLink, opts AddOptions) ([]*Item, error) {
	password := opts.Password
	for _, l := range links {
		if l.Password != "" {
			password = l.Password
			break
		}
	}

	if pkgID == "" {
		pkg := &Package{ID: newID(), Name: packageNameFromLinks(links), Password: password, CreatedAt: time.Now()}
		if err := m.store.PutPackage(pkg); err != nil {
			return nil, fmt.Errorf("queue: persist package: %w", err)
		}
		m.mu.Lock()
		m.packages[pkg.ID] = pkg
		m.mu.Unlock()
		pkgCopy := *pkg
		m.publish(Event{Type: EventPackageUpdated, Package: &pkgCopy})
		pkgID = pkg.ID
	} else if password != "" {
		m.mu.Lock()
		pkg, ok := m.packages[pkgID]
		var pkgCopy Package
		if ok {
			if pkg.Password == "" {
				pkg.Password = password
			}
			pkgCopy = *pkg
		}
		m.mu.Unlock()
		if ok {
			if err := m.store.PutPackage(&pkgCopy); err != nil {
				return nil, fmt.Errorf("queue: persist package: %w", err)
			}
			m.publish(Event{Type: EventPackageUpdated, Package: &pkgCopy})
		}
	}

	items := make([]*Item, 0, len(links))
	var firstErr error
	for _, l := range links {
		itemOpts := opts
		itemOpts.Filename = l.Name
		itemOpts.Password = l.Password
		itemOpts.Size = l.Size
		it, err := m.addPlainItem(pkgID, l.URL, itemOpts)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("queue: expansion produced no valid items: %w", firstErr)
	}
	return items, nil
}

// packageNameFromLinks derives a package name for a newly-expanded
// container from its first link's filename, stripping any multi-part
// suffix — extract.GroupBase already does exactly this for the
// auto-extract group-detection logic elsewhere in this file. The
// closest thing to a "release name" available here: plugins.ExpandedLink
// carries only a per-file Name, not a separate container-level title.
func packageNameFromLinks(links []plugins.ExpandedLink) string {
	if len(links) == 0 {
		return "expanded"
	}
	return extract.GroupBase(links[0].Name)
}

// addPlainItem is the original single-URL-in/single-item-out insertion
// logic: build one Item for rawURL and enqueue it. Used directly for the
// common case, and once per link by addExpandedItems.
func (m *Manager) addPlainItem(pkgID, rawURL string, opts AddOptions) (*Item, error) {
	if err := validHTTPURL(rawURL); err != nil {
		return nil, err
	}

	hosterKey := opts.HosterKey
	if hosterKey == "" {
		if h, ok := m.matchHoster(rawURL); ok {
			hosterKey = h.Name()
		}
	}

	initialState := StateQueued
	if opts.StartPaused {
		initialState = StatePaused
	}

	now := time.Now()
	it := &Item{
		ID:          newID(),
		PackageID:   pkgID,
		URL:         rawURL,
		Filename:    opts.Filename,
		Password:    opts.Password,
		HosterKey:   hosterKey,
		Priority:    opts.Priority,
		State:       initialState,
		Size:        opts.Size,
		ScheduledAt: opts.ScheduledAt,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// Persisted and snapshotted under the lock: once it is in m.items a
	// concurrent dispatch can pick it up and mutate it, so neither the
	// store write nor the published event/return value may read the live
	// pointer afterwards.
	m.mu.Lock()
	it.Position = m.posSeq
	if err := m.store.PutItem(it); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("queue: persist item: %w", err)
	}
	m.posSeq++
	m.items[it.ID] = it
	cp := *it
	m.mu.Unlock()
	m.publish(Event{Type: EventItemUpdated, Item: &cp})

	m.runMu.Lock()
	runCtx := m.runCtx
	m.runMu.Unlock()
	if runCtx != nil {
		if cp.ScheduledAt != nil && cp.ScheduledAt.After(now) {
			m.armSchedule(runCtx, *cp.ScheduledAt)
		} else {
			m.wakeAsync()
		}
	}

	return &cp, nil
}

// requeue transitions id from any of fromStates into StateQueued.
func (m *Manager) requeue(id string, fromStates ...State) error {
	n, errs := m.requeueMany([]string{id}, fromStates...)
	if n == 0 && len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// requeueMany transitions every id in ids that is in one of fromStates
// into StateQueued — all under one lock, followed by a single dispatcher
// wake. Requeueing items one by one (each with its own wake) let the
// dispatcher start whichever item happened to be requeued first, so a
// "resume all" started downloads in random order; done atomically, the
// dispatcher sees them all at once and starts them top to bottom (see
// queueOrder). Returns how many items changed and one error per id that
// couldn't be requeued.
func (m *Manager) requeueMany(ids []string, fromStates ...State) (int, []error) {
	var errs []error
	var updated []*Item
	now := time.Now()

	m.mu.Lock()
	for _, id := range ids {
		it, ok := m.items[id]
		if !ok {
			errs = append(errs, fmt.Errorf("%w: %s", ErrNotFound, id))
			continue
		}
		if !slices.Contains(fromStates, it.State) {
			errs = append(errs, fmt.Errorf("queue: cannot requeue item in state %s", it.State))
			continue
		}
		// A manual resume/retry starts a fresh automatic-retry budget and
		// shouldn't sit out a leftover auto-retry wait.
		it.Attempts = 0
		it.RetryAfter = nil
		it.State = StateQueued
		it.Error = ""
		it.UpdatedAt = now
		if err := m.store.PutItem(it); err != nil {
			errs = append(errs, fmt.Errorf("queue: persist item: %w", err))
			continue
		}
		cp := *it
		updated = append(updated, &cp)
	}
	m.mu.Unlock()

	for _, it := range updated {
		m.publish(Event{Type: EventItemUpdated, Item: it})
	}
	if len(updated) > 0 {
		m.wakeAsync()
	}
	return len(updated), errs
}

// Resume requeues a paused item.
func (m *Manager) Resume(id string) error { return m.requeue(id, StatePaused) }

// Retry requeues a failed item.
func (m *Manager) Retry(id string) error { return m.requeue(id, StateError) }

// Pause stops a queued or in-flight item and marks it StatePaused. Resume
// requeues it; the downloader's own resume state means an in-flight pause
// loses at most the last unsaved fraction of a chunk.
func (m *Manager) Pause(id string) error {
	m.mu.Lock()
	it, ok := m.items[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	if it.State != StateDownloading && it.State != StateQueued {
		state := it.State
		m.mu.Unlock()
		return fmt.Errorf("queue: cannot pause item in state %s", state)
	}
	wasDownloading := it.State == StateDownloading
	it.State = StatePaused
	it.UpdatedAt = time.Now()
	if err := m.store.PutItem(it); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("queue: persist item: %w", err)
	}
	cp := *it
	m.mu.Unlock()

	if wasDownloading {
		m.activeMu.Lock()
		if h, ok := m.active[id]; ok {
			h.stop("pause")
		}
		m.activeMu.Unlock()
	}
	m.publish(Event{Type: EventItemUpdated, Item: &cp})
	return nil
}

// PauseAll pauses every currently queued or downloading item, via the
// same Pause used for a single item — reused rather than reimplemented,
// so the cancel-in-flight and persistence semantics can't drift between
// the single- and bulk- paths. Errors pausing individual items (e.g. one
// finished between the snapshot and the call) are collected, not fatal
// to the rest.
func (m *Manager) PauseAll() error {
	m.mu.RLock()
	ids := make([]string, 0, len(m.items))
	for _, it := range m.items {
		if it.State == StateQueued || it.State == StateDownloading {
			ids = append(ids, it.ID)
		}
	}
	m.mu.RUnlock()

	var errs []error
	for _, id := range ids {
		if err := m.Pause(id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ResumeAll requeues every currently paused item in one step (see
// requeueMany), so they start top to bottom.
func (m *Manager) ResumeAll() error {
	m.mu.RLock()
	ids := make([]string, 0, len(m.items))
	for _, it := range m.items {
		if it.State == StatePaused {
			ids = append(ids, it.ID)
		}
	}
	m.mu.RUnlock()

	_, errs := m.requeueMany(ids, StatePaused)
	return errors.Join(errs...)
}

// SetParallelFiles changes the maximum number of concurrently
// downloading items live (see internal/settings). Already-running
// downloads are unaffected; a lowered limit simply stops new ones from
// starting until activeFiles drops back under it (naturally, as running
// ones finish), a raised limit is picked up on the next dispatch.
func (m *Manager) SetParallelFiles(n int) {
	if n < 1 {
		n = 1
	}
	m.parallelFiles.Store(int64(n))
	m.wakeAsync()
}

// SetPriority updates an item's priority and wakes the dispatcher so a
// now-higher-priority item is considered on the very next pick, without
// waiting for the poll interval. Takes effect immediately; an
// already-downloading item's current transfer is unaffected (there's
// nothing to reorder while it's the one running), but it'll be
// reflected the moment it's next picked (e.g. after a pause/resume).
func (m *Manager) SetPriority(id string, p Priority) error {
	m.mu.Lock()
	it, ok := m.items[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	it.Priority = p
	it.UpdatedAt = time.Now()
	if err := m.store.PutItem(it); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("queue: persist item: %w", err)
	}
	cp := *it
	m.mu.Unlock()

	m.publish(Event{Type: EventItemUpdated, Item: &cp})
	m.wakeAsync()
	return nil
}

// SetPackagePath sets or clears a package's "save to" override (see
// packageDir). Already-queued items in the package pick this up the
// next time they're resolved (relPathFor reads pkg.Path live from
// m.packages, the same late-binding pattern matchHoster uses for
// account changes) — no requeue needed, though an item that's already
// mid-download keeps writing to wherever it already started.
func (m *Manager) SetPackagePath(id, path string) (*Package, error) {
	m.mu.Lock()
	pkg, ok := m.packages[id]
	if !ok {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	pkg.Path = path
	cp := *pkg
	m.mu.Unlock()

	if err := m.store.PutPackage(&cp); err != nil {
		return nil, fmt.Errorf("queue: persist package: %w", err)
	}
	m.publish(Event{Type: EventPackageUpdated, Package: &cp})
	return &cp, nil
}

// Cancel permanently removes an item from the queue, stopping any
// in-flight transfer. It does not delete partially downloaded files on
// disk.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	it, ok := m.items[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	wasDownloading := it.State == StateDownloading
	pkgID := it.PackageID
	// Partial data of an unfinished item that isn't running right now is
	// removed here; a running one is cleaned up by run() once its transfer
	// has stopped (see discardIfCancelled). The path needs a known filename.
	discardNow := !wasDownloading && it.State != StateDone && it.Filename != ""
	var relPath string
	if discardNow {
		relPath = m.relPathLocked(it)
	}
	delete(m.items, id)
	m.mu.Unlock()
	m.forgetFinished("item:" + id)

	if discardNow {
		if err := m.engine.DiscardPartial(relPath); err != nil {
			m.log.Warn("queue: remove partial download", "item_id", id, "error", err)
		}
	}

	if err := m.store.DeleteItem(id); err != nil {
		return fmt.Errorf("queue: delete item: %w", err)
	}
	defer m.removePackageIfEmpty(pkgID)

	if wasDownloading {
		m.activeMu.Lock()
		if h, ok := m.active[id]; ok {
			h.stop("cancel")
		}
		m.activeMu.Unlock()
	}
	m.publish(Event{Type: EventItemRemoved, Item: &Item{ID: id}})
	return nil
}

// CancelMany cancels every id in ids via the same Cancel used for a
// single item, continuing past individual failures (e.g. an id that no
// longer exists) rather than stopping at the first one. Used by the
// bulk DELETE /api/v1/queue endpoint so clearing N items — "Clear done"
// on a large queue, or a multi-select bulk cancel — is one request/one
// rate-limit hit instead of N parallel ones, which is what used to make
// a big-enough batch trip the API's rate limiter (see docs/ARCHITECTURE.md).
func (m *Manager) CancelMany(ids []string) (deleted []string, failed []string) {
	deleted = make([]string, 0, len(ids))
	failed = make([]string, 0)
	for _, id := range ids {
		if err := m.Cancel(id); err != nil {
			m.log.Warn("queue: bulk cancel: item failed", "item_id", id, "error", err)
			failed = append(failed, id)
			continue
		}
		deleted = append(deleted, id)
	}
	return deleted, failed
}

// Reorder assigns sequential Position values to the given items in the
// order supplied, for drag-to-reorder UIs. IDs not present in the queue
// are ignored; items not mentioned keep their existing Position.
func (m *Manager) Reorder(ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, id := range ids {
		it, ok := m.items[id]
		if !ok {
			continue
		}
		it.Position = int64(i)
		it.UpdatedAt = time.Now()
		if err := m.store.PutItem(it); err != nil {
			return fmt.Errorf("queue: persist item: %w", err)
		}
	}
	return nil
}

// List returns a snapshot of all items, ordered the same way the
// dispatcher prioritizes them.
func (m *Manager) List() []*Item {
	m.mu.RLock()
	order := newQueueOrder(m.items)
	out := make([]*Item, 0, len(m.items))
	for _, it := range m.items {
		cp := *it
		out = append(out, &cp)
	}
	m.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return order.less(out[i], out[j]) })
	return out
}

// Get returns a snapshot of a single item.
func (m *Manager) Get(id string) (*Item, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	it, ok := m.items[id]
	if !ok {
		return nil, false
	}
	cp := *it
	return &cp, true
}

// ListPackages returns a snapshot of all packages.
func (m *Manager) ListPackages() []*Package {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Package, 0, len(m.packages))
	for _, p := range m.packages {
		cp := *p
		out = append(out, &cp)
	}
	return out
}

// Subscribe registers for queue Events. The returned cancel func must be
// called when the subscriber is done to release its channel.
func (m *Manager) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	m.subMu.Lock()
	id := m.nextSub
	m.nextSub++
	m.subs[id] = ch
	m.subMu.Unlock()

	cancel := func() {
		m.subMu.Lock()
		if _, ok := m.subs[id]; ok {
			delete(m.subs, id)
			close(ch)
		}
		m.subMu.Unlock()
	}
	return ch, cancel
}

func (m *Manager) publish(ev Event) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	for _, ch := range m.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// removePackageIfEmpty deletes package id (from memory and the store) and
// publishes EventPackageRemoved if no item references it any more —
// called after an item is cancelled/removed, so "Clear done" or deleting
// a whole package doesn't leave an empty package record behind forever
// (they used to accumulate in bbolt and in GET /api/v1/packages).
func (m *Manager) removePackageIfEmpty(id string) {
	if id == "" {
		return
	}
	m.mu.Lock()
	if _, ok := m.packages[id]; !ok {
		m.mu.Unlock()
		return
	}
	for _, it := range m.items {
		if it.PackageID == id {
			m.mu.Unlock()
			return
		}
	}
	delete(m.packages, id)
	m.forgetFinished(id)
	if err := m.store.DeletePackage(id); err != nil {
		m.log.Error("queue: delete package", "package_id", id, "error", err)
	}
	m.mu.Unlock()
	m.publish(Event{Type: EventPackageRemoved, Package: &Package{ID: id}})
}

// PackageAction is a bulk action applied to every item of one package.
type PackageAction string

const (
	PackageActionPause  PackageAction = "pause"
	PackageActionResume PackageAction = "resume"
	PackageActionRetry  PackageAction = "retry"
	PackageActionDelete PackageAction = "delete"
)

// ErrInvalidAction is returned by ApplyPackageAction for an unknown action.
var ErrInvalidAction = errors.New("queue: invalid package action")

// ApplyPackageAction applies action to every item in package id via the
// same single-item methods (Pause/Resume/Retry/Cancel) the per-item API
// uses, so persistence and cancel-in-flight semantics can't drift.
// Items the action doesn't apply to (pausing a finished item, retrying
// one that didn't fail) are skipped silently; it returns how many items
// were changed. Deleting the last item removes the package itself (see
// removePackageIfEmpty).
func (m *Manager) ApplyPackageAction(id string, action PackageAction) (int, error) {
	m.mu.RLock()
	_, ok := m.packages[id]
	var ids []string
	for _, it := range m.items {
		if it.PackageID == id {
			ids = append(ids, it.ID)
		}
	}
	m.mu.RUnlock()
	if !ok {
		return 0, ErrNotFound
	}
	return m.ApplyAction(ids, action)
}

// ApplyAction applies action to every item in ids — used for package
// actions and for multi-select in the Web UI (POST /api/v1/queue/actions).
// Items the action doesn't apply to are skipped; it returns how many
// changed. Resume and retry requeue all affected items in one step (see
// requeueMany), so they start top to bottom instead of in request order.
func (m *Manager) ApplyAction(ids []string, action PackageAction) (int, error) {
	switch action {
	case PackageActionResume, PackageActionRetry:
		from := StatePaused
		if action == PackageActionRetry {
			from = StateError
		}
		m.mu.RLock()
		eligible := make([]string, 0, len(ids))
		for _, id := range ids {
			if it, ok := m.items[id]; ok && it.State == from {
				eligible = append(eligible, id)
			}
		}
		m.mu.RUnlock()
		n, errs := m.requeueMany(eligible, from)
		for _, err := range errs {
			m.log.Warn("queue: action: item failed", "action", action, "error", err)
		}
		return n, nil
	case PackageActionPause, PackageActionDelete:
	default:
		return 0, ErrInvalidAction
	}

	changed := 0
	for _, id := range ids {
		var err error
		if action == PackageActionPause {
			m.mu.RLock()
			it, ok := m.items[id]
			pausable := ok && (it.State == StateQueued || it.State == StateDownloading)
			m.mu.RUnlock()
			if !pausable {
				continue
			}
			err = m.Pause(id)
		} else {
			err = m.Cancel(id)
		}
		if err != nil {
			m.log.Warn("queue: action: item failed", "action", action, "item_id", id, "error", err)
			continue
		}
		changed++
	}
	return changed, nil
}

// RenamePackage changes package id's display name. Items keep
// downloading into the directory they were resolved against; the new name
// applies to items whose download starts afterwards (same late binding as
// SetPackagePath).
func (m *Manager) RenamePackage(id, name string) (*Package, error) {
	m.mu.Lock()
	pkg, ok := m.packages[id]
	if !ok {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	pkg.Name = name
	cp := *pkg
	if err := m.store.PutPackage(&cp); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("queue: persist package: %w", err)
	}
	m.mu.Unlock()
	m.publish(Event{Type: EventPackageUpdated, Package: &cp})
	return &cp, nil
}

// discardIfCancelled removes the partial data of a transfer that stopped
// because its item was cancelled (removed from the queue) — the user no
// longer wants it, and multi-GB .part files would otherwise linger.
// Paused or shut-down transfers keep their data for resuming.
func (m *Manager) discardIfCancelled(h *runHandle, id, relPath string) {
	m.activeMu.Lock()
	cancelled := h.intent == "cancel"
	m.activeMu.Unlock()
	if !cancelled {
		return
	}
	if err := m.engine.DiscardPartial(relPath); err != nil {
		m.log.Warn("queue: remove partial download", "item_id", id, "error", err)
	}
}

// relPathLocked is relPathFor for a caller already holding m.mu.
// it.Filename must be set.
func (m *Manager) relPathLocked(it *Item) string {
	return m.joinPackageDir(it.PackageID, it.Filename)
}

// joinPackageDir returns name (sanitized) inside package pkgID's download
// directory. Caller holds m.mu.
func (m *Manager) joinPackageDir(pkgID, name string) string {
	name = pathsafe.SanitizeFilename(name)
	pkg, ok := m.packages[pkgID]
	if !ok {
		return name
	}
	if dir := packageDir(pkg); dir != "" {
		return dir + "/" + name
	}
	return name
}
