package queue

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/simuuh/hopper-downloader/internal/downloader"
	"github.com/simuuh/hopper-downloader/internal/extract"
	"github.com/simuuh/hopper-downloader/internal/scheduler"
	"github.com/simuuh/hopper-downloader/plugins"
	"github.com/simuuh/hopper-downloader/plugins/directhttp"
)

// fakeExtractor records every Extract call instead of shelling out to a
// real unrar / doing real archive I/O.
type fakeExtractor struct {
	mu    sync.Mutex
	calls []extractCall
	err   error
}

type extractCall struct {
	srcPath, destDir, pkgPassword string
	userPasswords                 []string
}

func (f *fakeExtractor) Extract(ctx context.Context, srcPath, destDir, pkgPassword string, userPasswords []string) (*extract.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, extractCall{srcPath, destDir, pkgPassword, userPasswords})
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &extract.Result{ExtractedFiles: []string{filepath.Join(destDir, "extracted.txt")}}, nil
}

func (f *fakeExtractor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeExtractor) lastCall() extractCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

type fakeExtractSettings struct {
	enabled     bool
	deleteAfter bool
	passwords   []string
}

func (f *fakeExtractSettings) ExtractEnabled() bool       { return f.enabled }
func (f *fakeExtractSettings) ExtractDeleteAfter() bool   { return f.deleteAfter }
func (f *fakeExtractSettings) ExtractPasswords() []string { return f.passwords }

func newExtractTestManager(t *testing.T, fe *fakeExtractor, fs *fakeExtractSettings, parallelFiles int) (*Manager, string) {
	t.Helper()
	baseDir := t.TempDir()
	store := newTestStore(t)
	reg := plugins.NewRegistry(directhttp.New())
	eng := downloader.New(downloader.Config{BaseDir: baseDir, RetryMax: 1, RetryBackoffBase: 5 * time.Millisecond})
	mgr, err := New(store, reg, eng, nil, nil, fe, fs, scheduler.New(), discardLogger(), Config{ParallelFiles: parallelFiles, PollInterval: 15 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return mgr, baseDir
}

func TestManager_AutoExtract_Disabled_NeverCalled(t *testing.T) {
	srv := staticFileServer(t, []byte("zip content"))
	fe := &fakeExtractor{}
	fs := &fakeExtractSettings{enabled: false}
	mgr, _ := newExtractTestManager(t, fe, fs, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.addItem("", srv.URL+"/archive.zip", AddOptions{Filename: "archive.zip"})
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, mgr, it.ID, StateDone, 2*time.Second)

	time.Sleep(100 * time.Millisecond)
	if fe.callCount() != 0 {
		t.Errorf("extractor called %d times with extraction disabled, want 0", fe.callCount())
	}
}

func TestManager_AutoExtract_SingleArchiveOnCompletion(t *testing.T) {
	srv := staticFileServer(t, []byte("zip content"))
	fe := &fakeExtractor{}
	fs := &fakeExtractSettings{enabled: true}
	mgr, _ := newExtractTestManager(t, fe, fs, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.addItem("", srv.URL+"/archive.zip", AddOptions{Filename: "archive.zip"})
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, mgr, it.ID, StateDone, 2*time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fe.callCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if fe.callCount() != 1 {
		t.Fatalf("extractor called %d times, want 1", fe.callCount())
	}
	if got := filepath.Base(fe.lastCall().srcPath); got != "archive.zip" {
		t.Errorf("srcPath basename = %q, want archive.zip", got)
	}
}

// TestManager_AutoExtract_MultiPartRAR_WaitsForAllParts is the core
// correctness case for maybeExtract's design: with ParallelFiles: 1 and
// part 2 added before part 1, part 2 completes first — "out of order"
// relative to the archive's own numbering — and extraction must not
// trigger until part 1 (the last to finish here) also completes, at
// which point it must run against part 1's path specifically (unrar
// finds the rest itself), regardless of completion order.
func TestManager_AutoExtract_MultiPartRAR_WaitsForAllParts(t *testing.T) {
	// Part 1's response blocks on release until explicitly let through,
	// so completion order is deterministic: part 2 (unblocked) finishes
	// first, part 1 finishes only once the test says so — giving a clean
	// checkpoint to verify maybeExtract does *not* trigger on part 2
	// alone before asserting it does once part 1 also completes.
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/p1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		if r.Method == http.MethodHead {
			return
		}
		<-release
		_, _ = w.Write([]byte("data"))
	})
	mux.HandleFunc("/p2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write([]byte("data"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	fe := &fakeExtractor{}
	fs := &fakeExtractSettings{enabled: true, deleteAfter: true}
	mgr, baseDir := newExtractTestManager(t, fe, fs, 2)

	pkg := &Package{ID: "pkg1", Name: "Archive", Password: "pkgpass", CreatedAt: time.Now()}
	mgr.mu.Lock()
	mgr.packages[pkg.ID] = pkg
	mgr.mu.Unlock()
	if err := mgr.store.PutPackage(pkg); err != nil {
		t.Fatal(err)
	}

	it1, err := mgr.addItem(pkg.ID, srv.URL+"/p1", AddOptions{Filename: "archive.part01.rar"})
	if err != nil {
		t.Fatal(err)
	}
	it2, err := mgr.addItem(pkg.ID, srv.URL+"/p2", AddOptions{Filename: "archive.part02.rar"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx) // ParallelFiles: 2 here so both parts dispatch concurrently; part 1 just blocks mid-transfer

	waitForState(t, mgr, it2.ID, StateDone, 2*time.Second)
	time.Sleep(100 * time.Millisecond) // let maybeExtract run (and, correctly, not trigger)
	if fe.callCount() != 0 {
		t.Fatalf("extractor called after only 1 of 2 parts done, want 0 calls")
	}

	close(release)
	waitForState(t, mgr, it1.ID, StateDone, 2*time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fe.callCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if fe.callCount() != 1 {
		t.Fatalf("extractor called %d times once both parts were done, want exactly 1 (no double-trigger)", fe.callCount())
	}
	call := fe.lastCall()
	if got := filepath.Base(call.srcPath); got != "archive.part01.rar" {
		t.Errorf("srcPath basename = %q, want the first part (archive.part01.rar) regardless of completion order", got)
	}
	if call.pkgPassword != "pkgpass" {
		t.Errorf("pkgPassword = %q, want pkgpass", call.pkgPassword)
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, err1 := os.Stat(filepath.Join(baseDir, "archive.part01.rar"))
		_, err2 := os.Stat(filepath.Join(baseDir, "archive.part02.rar"))
		if os.IsNotExist(err1) && os.IsNotExist(err2) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected both archive parts to be deleted after successful extraction (ExtractDeleteAfter: true)")
}

func TestManager_AutoExtract_KeepsArchiveWhenDeleteAfterFalse(t *testing.T) {
	srv := staticFileServer(t, []byte("zip content"))
	fe := &fakeExtractor{}
	fs := &fakeExtractSettings{enabled: true, deleteAfter: false}
	mgr, baseDir := newExtractTestManager(t, fe, fs, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.addItem("", srv.URL+"/archive.zip", AddOptions{Filename: "archive.zip"})
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, mgr, it.ID, StateDone, 2*time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fe.callCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if fe.callCount() != 1 {
		t.Fatalf("extractor called %d times, want 1", fe.callCount())
	}

	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(baseDir, "archive.zip")); err != nil {
		t.Errorf("archive.zip should still exist with ExtractDeleteAfter: false, stat error: %v", err)
	}
}

// TestManager_ResetsInterruptedExtractionsOnStart covers the recovery
// path for an item that was mid-extraction when hopperd was last killed:
// runExtraction only ever runs while the process that started it is
// alive, so an item still showing ExtractStateExtracting when a new
// Manager starts didn't finish — it just never got the chance to report
// success or failure. Start must reset it to extract_failed (with a
// message pointing at the re-extract button) rather than leaving it
// stuck as "Extracting…" forever, since RetryExtraction only accepts an
// item already in ExtractStateExtractFailed.
func TestManager_ResetsInterruptedExtractionsOnStart(t *testing.T) {
	fe := &fakeExtractor{}
	fs := &fakeExtractSettings{enabled: true}
	mgr, _ := newExtractTestManager(t, fe, fs, 1)

	it := &Item{ID: newID(), Filename: "archive.zip", ExtractState: ExtractStateExtracting}
	injectDoneItem(t, mgr, it)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	got, ok := mgr.Get(it.ID)
	if !ok {
		t.Fatal("item not found after Start")
	}
	if got.ExtractState != ExtractStateExtractFailed {
		t.Errorf("ExtractState = %q, want %q", got.ExtractState, ExtractStateExtractFailed)
	}
	if got.ExtractError != "extraction interrupted by restart — use re-extract button" {
		t.Errorf("ExtractError = %q, want the restart-interrupted message", got.ExtractError)
	}
	if fe.callCount() != 0 {
		t.Errorf("extractor called %d times, want 0 (this is a state fixup, not a real extraction)", fe.callCount())
	}
}

// TestManager_ExtractTriggeredClearedAfterCompletion covers the fix for
// a real bug: extractTriggered previously had no eviction path at all,
// so deleting every item in an archive group and re-adding the same
// archive would never trigger extraction again. runExtraction must clear
// its group's dedup entry once it's done, success or failure.
func TestManager_ExtractTriggeredClearedAfterCompletion(t *testing.T) {
	srv := staticFileServer(t, []byte("zip content"))
	fe := &fakeExtractor{}
	fs := &fakeExtractSettings{enabled: true}
	mgr, _ := newExtractTestManager(t, fe, fs, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	it, err := mgr.addItem("", srv.URL+"/archive.zip", AddOptions{Filename: "archive.zip"})
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, mgr, it.ID, StateDone, 2*time.Second)
	waitForExtractState(t, mgr, it.ID)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mgr.extractTriggeredMu.Lock()
		n := len(mgr.extractTriggered)
		mgr.extractTriggeredMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected extractTriggered to be cleared once runExtraction completed")
}

func TestAllPartsOnDisk(t *testing.T) {
	fe := &fakeExtractor{}
	fs := &fakeExtractSettings{enabled: true}
	mgr, baseDir := newExtractTestManager(t, fe, fs, 1)

	if err := os.WriteFile(filepath.Join(baseDir, "archive.part01.rar"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// archive.part02.rar deliberately never written.

	ok, err := mgr.allPartsOnDisk([]*Item{{Filename: "archive.part01.rar"}})
	if err != nil || !ok {
		t.Errorf("allPartsOnDisk (present) = %v, %v, want true, nil", ok, err)
	}

	ok, err = mgr.allPartsOnDisk([]*Item{
		{Filename: "archive.part01.rar"},
		{Filename: "archive.part02.rar"},
	})
	if err != nil {
		t.Fatalf("allPartsOnDisk (one missing): unexpected error %v", err)
	}
	if ok {
		t.Error("allPartsOnDisk = true, want false: archive.part02.rar was never written")
	}
}

// TestManager_AutoExtract_MissingPartOnDiskFailsWithoutExtracting covers
// the case allPartsOnDisk exists for: every sibling item in the group
// reports StateDone (maybeExtract's normal gate), but one part's file
// isn't actually present on disk. it2 is injected directly as an
// already-Done item whose file was never written, rather than
// downloaded-then-deleted, so the test is deterministic instead of
// racing runExtraction's own disk-check goroutine.
func TestManager_AutoExtract_MissingPartOnDiskFailsWithoutExtracting(t *testing.T) {
	origAttempts, origInterval := diskCheckAttempts, diskCheckInterval
	diskCheckAttempts, diskCheckInterval = 2, 5*time.Millisecond
	t.Cleanup(func() { diskCheckAttempts, diskCheckInterval = origAttempts, origInterval })

	srv := staticFileServer(t, []byte("data"))
	fe := &fakeExtractor{}
	fs := &fakeExtractSettings{enabled: true}
	mgr, _ := newExtractTestManager(t, fe, fs, 1)

	pkg := &Package{ID: "pkg1", Name: "Archive", CreatedAt: time.Now()}
	mgr.mu.Lock()
	mgr.packages[pkg.ID] = pkg
	mgr.mu.Unlock()
	if err := mgr.store.PutPackage(pkg); err != nil {
		t.Fatal(err)
	}

	it1, err := mgr.addItem(pkg.ID, srv.URL+"/archive.part01.rar", AddOptions{Filename: "archive.part01.rar"})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	it2 := &Item{
		ID: newID(), PackageID: pkg.ID, URL: "http://example.invalid/p2",
		Filename: "archive.part02.rar", State: StateDone, CreatedAt: now, UpdatedAt: now,
	}
	mgr.mu.Lock()
	it2.Position = mgr.posSeq
	mgr.posSeq++
	mgr.items[it2.ID] = it2
	mgr.mu.Unlock()
	if err := mgr.store.PutItem(it2); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	waitForState(t, mgr, it1.ID, StateDone, 2*time.Second)

	deadline := time.Now().Add(2 * time.Second)
	var final *Item
	for time.Now().Before(deadline) {
		it, ok := mgr.Get(it1.ID)
		if ok && it.ExtractState == ExtractStateExtractFailed {
			final = it
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if final == nil {
		t.Fatal("expected ExtractState to become extract_failed")
	}
	if fe.callCount() != 0 {
		t.Errorf("extractor called %d times, want 0 (a missing part must block extraction entirely)", fe.callCount())
	}
}

// injectDoneItem adds it directly to mgr's in-memory/persisted state as
// StateDone, bypassing a real download — used by the RetryExtraction
// tests below, which only care about post-completion behavior.
func injectDoneItem(t *testing.T, mgr *Manager, it *Item) {
	t.Helper()
	now := time.Now()
	it.State = StateDone
	it.CreatedAt = now
	it.UpdatedAt = now
	mgr.mu.Lock()
	it.Position = mgr.posSeq
	mgr.posSeq++
	mgr.items[it.ID] = it
	mgr.mu.Unlock()
	if err := mgr.store.PutItem(it); err != nil {
		t.Fatal(err)
	}
}

func waitForExtractState(t *testing.T, mgr *Manager, id string) *Item {
	t.Helper()
	const want = ExtractStateExtracted
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		it, ok := mgr.Get(id)
		if ok && it.ExtractState == want {
			return it
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("item %s never reached ExtractState %q", id, want)
	return nil
}

func TestRetryExtraction_NotFound(t *testing.T) {
	fe := &fakeExtractor{}
	fs := &fakeExtractSettings{enabled: true}
	mgr, _ := newExtractTestManager(t, fe, fs, 1)

	if err := mgr.RetryExtraction("missing-id", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRetryExtraction_NotDoneRejected(t *testing.T) {
	fe := &fakeExtractor{}
	fs := &fakeExtractSettings{enabled: true}
	mgr, _ := newExtractTestManager(t, fe, fs, 1)

	it := &Item{ID: newID(), Filename: "archive.zip", ExtractState: ExtractStateExtractFailed}
	injectDoneItem(t, mgr, it)
	// Override the StateDone injectDoneItem forces, to exercise the
	// "still downloading" (or otherwise not-done) rejection path.
	mgr.mu.Lock()
	mgr.items[it.ID].State = StateQueued
	mgr.mu.Unlock()

	if err := mgr.RetryExtraction(it.ID, ""); !errors.Is(err, ErrExtractNotRetryable) {
		t.Errorf("err = %v, want ErrExtractNotRetryable", err)
	}
	if fe.callCount() != 0 {
		t.Errorf("extractor called %d times, want 0", fe.callCount())
	}
}

func TestRetryExtraction_NotExtractFailedRejected(t *testing.T) {
	fe := &fakeExtractor{}
	fs := &fakeExtractSettings{enabled: true}
	mgr, _ := newExtractTestManager(t, fe, fs, 1)

	it := &Item{ID: newID(), Filename: "archive.zip"} // ExtractState is the zero value ""
	injectDoneItem(t, mgr, it)

	if err := mgr.RetryExtraction(it.ID, ""); !errors.Is(err, ErrExtractNotRetryable) {
		t.Errorf("err = %v, want ErrExtractNotRetryable", err)
	}
	if fe.callCount() != 0 {
		t.Errorf("extractor called %d times, want 0", fe.callCount())
	}
}

func TestRetryExtraction_ArchiveMissingOnDisk(t *testing.T) {
	fe := &fakeExtractor{}
	fs := &fakeExtractSettings{enabled: true}
	mgr, _ := newExtractTestManager(t, fe, fs, 1)

	// Deliberately never written to baseDir.
	it := &Item{ID: newID(), Filename: "archive.zip", ExtractState: ExtractStateExtractFailed}
	injectDoneItem(t, mgr, it)

	if err := mgr.RetryExtraction(it.ID, ""); !errors.Is(err, ErrArchiveMissing) {
		t.Errorf("err = %v, want ErrArchiveMissing", err)
	}
	if fe.callCount() != 0 {
		t.Errorf("extractor called %d times, want 0", fe.callCount())
	}
}

func TestRetryExtraction_Success(t *testing.T) {
	fe := &fakeExtractor{}
	fs := &fakeExtractSettings{enabled: true, passwords: []string{"settingspw"}}
	mgr, baseDir := newExtractTestManager(t, fe, fs, 1)

	if err := os.WriteFile(filepath.Join(baseDir, "archive.zip"), []byte("zip content"), 0o600); err != nil {
		t.Fatal(err)
	}

	it := &Item{ID: newID(), Filename: "archive.zip", ExtractState: ExtractStateExtractFailed, ExtractError: "wrong password"}
	injectDoneItem(t, mgr, it)

	events, unsubscribe := mgr.Subscribe()
	defer unsubscribe()

	if err := mgr.RetryExtraction(it.ID, "retrypw"); err != nil {
		t.Fatalf("RetryExtraction: %v", err)
	}

	// The reset to "" is published synchronously, before the background
	// extraction starts. Checked via the first event rather than Get():
	// the goroutine may already have set "extracting" by the time Get
	// runs (the archive is on disk, so it doesn't wait).
	select {
	case ev := <-events:
		if ev.Type != EventItemUpdated || ev.Item == nil || ev.Item.ID != it.ID || ev.Item.ExtractState != "" {
			t.Errorf("first event after RetryExtraction = %+v, want item_updated with ExtractState cleared", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event after RetryExtraction")
	}

	waitForExtractState(t, mgr, it.ID)

	if fe.callCount() != 1 {
		t.Fatalf("extractor called %d times, want 1", fe.callCount())
	}
	call := fe.lastCall()
	if len(call.userPasswords) == 0 || call.userPasswords[0] != "retrypw" {
		t.Errorf("userPasswords = %v, want retrypw prepended ahead of the settings list", call.userPasswords)
	}
	found := false
	for _, p := range call.userPasswords {
		if p == "settingspw" {
			found = true
		}
	}
	if !found {
		t.Errorf("userPasswords = %v, want settingspw still present", call.userPasswords)
	}
}

func TestRetryExtraction_MultiPartUsesFirstPartAndPackagePassword(t *testing.T) {
	fe := &fakeExtractor{}
	fs := &fakeExtractSettings{enabled: true}
	mgr, baseDir := newExtractTestManager(t, fe, fs, 1)

	pkg := &Package{ID: "pkg1", Name: "Archive", Password: "pkgpass", CreatedAt: time.Now()}
	mgr.mu.Lock()
	mgr.packages[pkg.ID] = pkg
	mgr.mu.Unlock()
	if err := mgr.store.PutPackage(pkg); err != nil {
		t.Fatal(err)
	}

	pkgDir := filepath.Join(baseDir, "Archive")
	if err := os.MkdirAll(pkgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"archive.part01.rar", "archive.part02.rar"} {
		if err := os.WriteFile(filepath.Join(pkgDir, name), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	it2 := &Item{ID: newID(), PackageID: pkg.ID, Filename: "archive.part02.rar", ExtractState: ExtractStateExtractFailed}
	injectDoneItem(t, mgr, it2)
	it1 := &Item{ID: newID(), PackageID: pkg.ID, Filename: "archive.part01.rar", ExtractState: ExtractStateExtractFailed}
	injectDoneItem(t, mgr, it1)

	// Retrying via either sibling's ID re-extracts the same group, always
	// from the first part.
	if err := mgr.RetryExtraction(it2.ID, ""); err != nil {
		t.Fatalf("RetryExtraction: %v", err)
	}

	waitForExtractState(t, mgr, it1.ID)
	waitForExtractState(t, mgr, it2.ID)

	if fe.callCount() != 1 {
		t.Fatalf("extractor called %d times, want 1", fe.callCount())
	}
	call := fe.lastCall()
	if got := filepath.Base(call.srcPath); got != "archive.part01.rar" {
		t.Errorf("srcPath basename = %q, want the first part", got)
	}
	if call.pkgPassword != "pkgpass" {
		t.Errorf("pkgPassword = %q, want pkgpass", call.pkgPassword)
	}
}
