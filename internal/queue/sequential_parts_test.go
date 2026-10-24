package queue

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// injectQueuedItem adds it directly to mgr's in-memory state as
// StateQueued, bypassing a real AddLink/AddPackage call — these tests
// only care about pickNext's admission-ordering decision, not plugin
// resolution or a real download.
func injectQueuedItem(t *testing.T, mgr *Manager, it *Item) {
	t.Helper()
	now := time.Now()
	it.State = StateQueued
	it.CreatedAt = now
	it.UpdatedAt = now
	mgr.mu.Lock()
	it.Position = mgr.posSeq
	mgr.posSeq++
	mgr.items[it.ID] = it
	mgr.mu.Unlock()
}

// pick is pickNext for tests that only look at which item is admitted.
func pick(mgr *Manager) *Item {
	it, _ := mgr.pickNext(context.Background())
	return it
}

func setState(mgr *Manager, it *Item, s State) {
	mgr.mu.Lock()
	it.State = s
	mgr.mu.Unlock()
}

func TestPickNext_SequentialParts_AdmitsLowestNumberedPartFirst(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 3, SequentialParts: true})

	// Inserted out of order (part3, part1, part2) and specifically *not*
	// in ascending Position order, so a pass with the ordering feature
	// disabled would pick part3 first (lowest Position, see lessItem) —
	// proving this is the sequential-parts logic doing the picking, not
	// an accident of insertion order.
	part3 := &Item{ID: newID(), PackageID: "pkg1", URL: "http://x/p3", Filename: "movie.part3.rar"}
	injectQueuedItem(t, mgr, part3)
	part1 := &Item{ID: newID(), PackageID: "pkg1", URL: "http://x/p1", Filename: "movie.part1.rar"}
	injectQueuedItem(t, mgr, part1)
	part2 := &Item{ID: newID(), PackageID: "pkg1", URL: "http://x/p2", Filename: "movie.part2.rar"}
	injectQueuedItem(t, mgr, part2)

	first := pick(mgr)
	if first == nil || first.ID != part1.ID {
		t.Fatalf("first pick = %+v, want part1", first)
	}

	// Parts may run side by side — they just never overtake each other.
	second := pick(mgr)
	if second == nil || second.ID != part2.ID {
		t.Fatalf("second pick = %+v, want part2 (part1 has started)", second)
	}
	third := pick(mgr)
	if third == nil || third.ID != part3.ID {
		t.Fatalf("third pick = %+v, want part3", third)
	}
}

func TestPickNext_SequentialParts_DoesNotBlockUnrelatedPackage(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 3, SequentialParts: true})

	// pkg1's part2 is (and stays, for now) blocked behind its still-queued
	// (then downloading) part1.
	part1 := &Item{ID: newID(), PackageID: "pkg1", URL: "http://x/p1", Filename: "movie.part1.rar"}
	injectQueuedItem(t, mgr, part1)
	part2 := &Item{ID: newID(), PackageID: "pkg1", URL: "http://x/p2", Filename: "movie.part2.rar"}
	injectQueuedItem(t, mgr, part2)
	// An unrelated package's plain single file, given priority so it
	// wins the very first pick regardless of insertion order — proving
	// pkg1's internal part-ordering gate has no effect whatsoever on
	// pkg2's own admission, not just that pkg2 "eventually" gets picked.
	other := &Item{ID: newID(), PackageID: "pkg2", URL: "http://x/other", Filename: "readme.txt", Priority: PriorityHigh}
	injectQueuedItem(t, mgr, other)

	first := pick(mgr)
	if first == nil || first.ID != other.ID {
		t.Fatalf("first pick = %+v, want the unrelated high-priority single-file item %s", first, other.ID)
	}

	// part1 is still queued (untouched by the pick above) and unblocked
	// (nothing lower-numbered in its own group), so it's next.
	second := pick(mgr)
	if second == nil || second.ID != part1.ID {
		t.Fatalf("second pick = %+v, want part1 %s", second, part1.ID)
	}

	third := pick(mgr)
	if third == nil || third.ID != part2.ID {
		t.Fatalf("third pick = %+v, want part2 %s", third, part2.ID)
	}
}

func TestPickNext_SequentialParts_ErroredLowerPartDoesNotStrandTheRest(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 3, SequentialParts: true})

	part1 := &Item{ID: newID(), PackageID: "pkg1", URL: "http://x/p1", Filename: "movie.part1.rar"}
	injectQueuedItem(t, mgr, part1)
	part2 := &Item{ID: newID(), PackageID: "pkg1", URL: "http://x/p2", Filename: "movie.part2.rar"}
	injectQueuedItem(t, mgr, part2)

	// part1 permanently failed — it must not block part2 forever, or a
	// single dead link would strand the rest of a large archive in the
	// queue with no way out short of manual intervention.
	setState(mgr, part1, StateError)

	picked := pick(mgr)
	if picked == nil || picked.ID != part2.ID {
		t.Fatalf("picked = %+v, want part2 (part1 errored, so it no longer blocks)", picked)
	}
}

func TestPickNext_SequentialParts_PausedLowerPartDoesNotStrandTheRest(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 3, SequentialParts: true})

	part1 := &Item{ID: newID(), PackageID: "pkg1", URL: "http://x/p1", Filename: "movie.part1.rar"}
	injectQueuedItem(t, mgr, part1)
	part2 := &Item{ID: newID(), PackageID: "pkg1", URL: "http://x/p2", Filename: "movie.part2.rar"}
	injectQueuedItem(t, mgr, part2)

	setState(mgr, part1, StatePaused)

	picked := pick(mgr)
	if picked == nil || picked.ID != part2.ID {
		t.Fatalf("picked = %+v, want part2 (part1 paused, so it no longer blocks)", picked)
	}
}

func TestPickNext_SequentialParts_LaterPartWaitsForNonRunnableLowerPart(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		store := newTestStore(t)
		mgr, _ := newTestManager(t, store, Config{ParallelFiles: 3, SequentialParts: enabled})

		// part1 is queued but waiting (e.g. a hoster's retry delay).
		later := time.Now().Add(time.Hour)
		part1 := &Item{ID: newID(), PackageID: "pkg1", URL: "http://x/p1", Filename: "movie.part1.rar", RetryAfter: &later}
		injectQueuedItem(t, mgr, part1)
		part2 := &Item{ID: newID(), PackageID: "pkg1", URL: "http://x/p2", Filename: "movie.part2.rar"}
		injectQueuedItem(t, mgr, part2)

		got := pick(mgr)
		if enabled && got != nil {
			t.Fatalf("enabled: picked %+v, want nil — part2 must not overtake waiting part1", got)
		}
		if !enabled && (got == nil || got.ID != part2.ID) {
			t.Fatalf("disabled: picked %+v, want part2", got)
		}
	}
}

func TestPickNext_SequentialParts_SingleFilePackageUnaffected(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 3, SequentialParts: true})

	it := &Item{ID: newID(), PackageID: "pkg1", URL: "http://x/movie.mkv", Filename: "movie.mkv"}
	injectQueuedItem(t, mgr, it)

	picked := pick(mgr)
	if picked == nil || picked.ID != it.ID {
		t.Fatalf("picked = %+v, want the only queued item %s", picked, it.ID)
	}
}

// downloadingFilenames returns the filenames of every currently-
// downloading item, for asserting on real concurrency under Start's
// dispatch loop (as opposed to the pickNext-only tests above, which
// never involve a real download engine at all).
func downloadingFilenames(mgr *Manager) []string {
	var out []string
	for _, it := range mgr.List() {
		if it.State == StateDownloading {
			out = append(out, it.Filename)
		}
	}
	return out
}

// TestDispatch_FillsTopmostPackageFirst is the user-visible contract:
// with parallel_files=3, the free slots go to the parts of the package
// at the top of the queue (in part order), not to the next packages.
func TestDispatch_FillsTopmostPackageFirst(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 3, PollInterval: 5 * time.Millisecond, SequentialParts: true})

	release := make(chan struct{})
	srv := slowServer(t, release)
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFn) // after slowServer's own cleanup: runs first (LIFO)

	// Package A arrives with its parts out of order; package B after it.
	if _, err := mgr.AddLinks([]LinkInput{
		{URL: srv.URL + "/a3", Filename: "alpha.part3.rar"},
		{URL: srv.URL + "/a1", Filename: "alpha.part1.rar"},
		{URL: srv.URL + "/a4", Filename: "alpha.part4.rar"},
		{URL: srv.URL + "/a2", Filename: "alpha.part2.rar"},
	}, "", AddOptions{StartPaused: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AddLinks([]LinkInput{
		{URL: srv.URL + "/b1", Filename: "beta.part1.rar"},
		{URL: srv.URL + "/b2", Filename: "beta.part2.rar"},
	}, "", AddOptions{StartPaused: true}); err != nil {
		t.Fatal(err)
	}

	mgr.Start(t.Context())
	if err := mgr.ResumeAll(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for len(downloadingFilenames(mgr)) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("downloading = %v, want 3", downloadingFilenames(mgr))
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond) // let any wrong extra admission show up
	got := downloadingFilenames(mgr)
	sort.Strings(got)
	want := []string{"alpha.part1.rar", "alpha.part2.rar", "alpha.part3.rar"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("downloading = %v, want %v (topmost package first, in part order)", got, want)
	}

	// List (what the Web UI shows) is in the same order.
	var names []string
	for _, it := range mgr.List() {
		names = append(names, it.Filename)
	}
	wantOrder := "alpha.part1.rar,alpha.part2.rar,alpha.part3.rar,alpha.part4.rar,beta.part1.rar,beta.part2.rar"
	if strings.Join(names, ",") != wantOrder {
		t.Fatalf("List order = %v, want %s", names, wantOrder)
	}
	releaseFn()
}

// A part merged into a package later (here: part1 after part2/3) still
// sorts and starts at its package's place, before later packages.
func TestQueueOrder_LaterMergedPartKeepsPackagePlace(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{})
	add := func(name string) {
		if _, err := mgr.AddLinks([]LinkInput{{URL: "http://host.example/" + name, Filename: name}}, "", AddOptions{StartPaused: true}); err != nil {
			t.Fatal(err)
		}
	}
	add("alpha.part2.rar")
	add("alpha.part3.rar")
	add("beta.part1.rar")
	add("alpha.part1.rar") // merged into alpha's package later
	var names []string
	for _, it := range mgr.List() {
		names = append(names, it.Filename)
	}
	want := "alpha.part1.rar,alpha.part2.rar,alpha.part3.rar,beta.part1.rar"
	if strings.Join(names, ",") != want {
		t.Fatalf("order = %v, want %s", names, want)
	}
}

// A high-priority item moves its whole package to the top.
func TestQueueOrder_PackagePriorityMovesWholePackage(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{})
	res1, _ := mgr.AddLinks([]LinkInput{{URL: "http://h.example/a1", Filename: "alpha.part1.rar"}, {URL: "http://h.example/a2", Filename: "alpha.part2.rar"}}, "", AddOptions{StartPaused: true})
	res2, _ := mgr.AddLinks([]LinkInput{{URL: "http://h.example/b1", Filename: "beta.part1.rar"}, {URL: "http://h.example/b2", Filename: "beta.part2.rar"}}, "", AddOptions{StartPaused: true})
	_ = res1
	if err := mgr.SetPriority(res2.Items[1].ID, PriorityHigh); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, it := range mgr.List() {
		names = append(names, it.Filename)
	}
	want := "beta.part2.rar,beta.part1.rar,alpha.part1.rar,alpha.part2.rar"
	if strings.Join(names, ",") != want {
		t.Fatalf("order = %v, want %s", names, want)
	}
}
