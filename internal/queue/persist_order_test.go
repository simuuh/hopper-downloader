package queue

import (
	"fmt"
	"testing"
	"time"
)

// When the database can't delete an item, Cancel fails and the item
// stays, instead of vanishing until the next start brings it back (N18).
func TestCancel_DatabaseFailureKeepsItem(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1})
	it := &Item{ID: newID(), Filename: "file.bin"}
	injectDoneItem(t, mgr, it)

	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Cancel(it.ID); err == nil {
		t.Fatal("Cancel succeeded with the database closed")
	}
	if _, ok := mgr.Get(it.ID); !ok {
		t.Fatal("item removed from the queue although the database still has it")
	}
}

// Reorder publishes every moved item, so other open Web UIs follow (N18).
func TestReorder_PublishesMovedItems(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{ParallelFiles: 1})
	a := &Item{ID: newID(), Filename: "a.bin"}
	b := &Item{ID: newID(), Filename: "b.bin"}
	injectDoneItem(t, mgr, a)
	injectDoneItem(t, mgr, b)

	events, unsubscribe := mgr.Subscribe()
	defer unsubscribe()
	if err := mgr.Reorder([]string{b.ID, a.ID}); err != nil {
		t.Fatal(err)
	}

	got := map[string]int64{}
	timeout := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-events:
			if ev.Type == EventItemUpdated && ev.Item != nil {
				got[ev.Item.ID] = ev.Item.Position
			}
		case <-timeout:
			t.Fatalf("got updates for %d of 2 items", len(got))
		}
	}
	if got[b.ID] != 0 || got[a.ID] != 1 {
		t.Fatalf("published positions %v, want b=0 a=1", got)
	}
}

// A pasted list goes into the database in one transaction: all items in
// their order, or none of them when the write fails (M27).
func TestAddLinks_PersistsAllOrNothing(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1})
	links := make([]LinkInput, 50)
	for i := range links {
		links[i] = LinkInput{URL: fmt.Sprintf("https://host.example/file%02d.bin", i)}
	}
	if _, err := mgr.AddLinks(links, "list", AddOptions{StartPaused: true}); err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.ListItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != len(links) {
		t.Fatalf("%d items in the database, want %d", len(stored), len(links))
	}
	byURL := map[string]int64{}
	for _, it := range stored {
		byURL[it.URL] = it.Position
	}
	for i := 1; i < len(links); i++ {
		if byURL[links[i].URL] <= byURL[links[i-1].URL] {
			t.Fatalf("link %d stored at position %d, not after link %d (%d)", i, byURL[links[i].URL], i-1, byURL[links[i-1].URL])
		}
	}

	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	before := len(mgr.List())
	var batch []*Item
	for _, u := range []string{"https://host.example/a.bin", "https://host.example/b.bin"} {
		it, err := mgr.newItem("", u, AddOptions{StartPaused: true})
		if err != nil {
			t.Fatal(err)
		}
		batch = append(batch, it)
	}
	if _, err := mgr.insertItems(batch); err == nil {
		t.Fatal("insertItems succeeded with the database closed")
	}
	if after := len(mgr.List()); after != before {
		t.Fatalf("%d items in the queue after a failed add, want %d", after, before)
	}
}

// A package's "save to" path is cleaned segment by segment (N5).
func TestSanitizePackagePath(t *testing.T) {
	cases := map[string]string{
		"":                        "",
		"Movies/2026":             "Movies/2026",
		`Movies\Series\`:          "Movies/Series",
		"a/./b/../c":              "a/b/c",
		"CON/name:stream/trail. ": "_CON/name_stream/trail",
		"/1/2/3/4/5/6/7/8/9/10":   "1/2/3/4/5/6/7/8",
	}
	for in, want := range cases {
		if got := sanitizePackagePath(in); got != want {
			t.Errorf("sanitizePackagePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// A download named like another one's temp or resume file is renamed so
// it can't overwrite it (M20).
func TestJoinPackageDir_AvoidsDownloaderWorkFiles(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{ParallelFiles: 1})
	cases := map[string]string{
		"movie.mkv.part":      "movie.mkv.part_",
		"movie.mkv.PART.json": "movie.mkv.PART.json_",
		"example.part1.rar":   "example.part1.rar",
	}
	for in, want := range cases {
		mgr.mu.RLock()
		got := mgr.joinPackageDir("", in)
		mgr.mu.RUnlock()
		if got != want {
			t.Errorf("joinPackageDir(%q) = %q, want %q", in, got, want)
		}
	}
}
