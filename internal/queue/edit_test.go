package queue

import (
	"errors"
	"testing"
)

func strp(s string) *string { return &s }

func TestEditItem_NewURLRestartsFailedItem(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{})
	it, err := mgr.AddLink("https://dead.example.com/file.bin", AddOptions{StartPaused: true})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a final failure (manager not started, so nothing runs).
	mgr.mu.Lock()
	mgr.items[it.ID].State = StateError
	mgr.items[it.ID].Error = "file unavailable at hoster"
	mgr.items[it.ID].Attempts = 3
	mgr.mu.Unlock()

	got, err := mgr.EditItem(it.ID, ItemEdit{URL: strp("  https://mirror.example.com/file.bin ")})
	if err != nil {
		t.Fatalf("EditItem: %v", err)
	}
	if got.URL != "https://mirror.example.com/file.bin" || got.State != StateQueued || got.Error != "" || got.Attempts != 0 {
		t.Errorf("edited item = %+v", got)
	}
	if got.HosterKey != "directhttp" {
		t.Errorf("HosterKey = %q, want re-matched directhttp", got.HosterKey)
	}
}

func TestEditItem_FilenameOverrideAndClear(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{})
	it, _ := mgr.AddLink("https://example.com/a", AddOptions{StartPaused: true})

	got, err := mgr.EditItem(it.ID, ItemEdit{Filename: strp("../movie.part1.rar")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Filename != "movie.part1.rar" {
		t.Errorf("Filename = %q, want sanitized movie.part1.rar", got.Filename)
	}
	if got.State != StatePaused {
		t.Errorf("state changed to %s by a filename-only edit", got.State)
	}
	got, _ = mgr.EditItem(it.ID, ItemEdit{Filename: strp("")})
	if got.Filename != "" {
		t.Errorf("Filename = %q, want cleared", got.Filename)
	}
}

func TestEditItem_RejectsDoneAndDuplicates(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{})
	a, _ := mgr.AddLink("https://example.com/a", AddOptions{StartPaused: true})
	b, _ := mgr.AddLink("https://example.com/b", AddOptions{StartPaused: true})

	if _, err := mgr.EditItem(b.ID, ItemEdit{URL: strp(a.URL)}); !errors.Is(err, ErrDuplicateURL) {
		t.Errorf("duplicate URL: err = %v, want ErrDuplicateURL", err)
	}
	if _, err := mgr.EditItem(b.ID, ItemEdit{URL: strp("ftp://example.com/x")}); err == nil {
		t.Error("non-http URL accepted")
	}

	mgr.mu.Lock()
	mgr.items[a.ID].State = StateDone
	mgr.mu.Unlock()
	if _, err := mgr.EditItem(a.ID, ItemEdit{Filename: strp("x")}); !errors.Is(err, ErrItemNotEditable) {
		t.Errorf("done item: err = %v, want ErrItemNotEditable", err)
	}
	if _, err := mgr.EditItem("nope", ItemEdit{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: err = %v, want ErrNotFound", err)
	}
}

func TestAddToPackage_AddsIntoGivenPackageAndSkipsDuplicates(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{})
	pkg, _, err := mgr.AddPackage("Release", []string{"https://h.example.com/movie.part1.rar"}, AddOptions{
		StartPaused: true,
		Filenames:   []string{"movie.part1.rar"},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := mgr.AddToPackage(pkg.ID, []LinkInput{
		{URL: "https://h.example.com/movie.part1.rar"},             // same URL
		{URL: "https://mirror.example.com/movie.part1.rar"},        // same filename = mirror
		{URL: "https://h.example.com/file/x/movie.part2.rar.html"}, // new part
		{URL: "https://h.example.com/notes.txt"},                   // unrelated file, still goes in
	}, AddOptions{StartPaused: true})
	if err != nil {
		t.Fatalf("AddToPackage: %v", err)
	}
	if len(res.Items) != 2 || len(res.Duplicates) != 2 {
		t.Fatalf("items %d duplicates %d, want 2/2: %+v", len(res.Items), len(res.Duplicates), res)
	}
	for _, it := range res.Items {
		if it.PackageID != pkg.ID {
			t.Errorf("item %s landed in package %q, want %q", it.URL, it.PackageID, pkg.ID)
		}
	}
	if res.Items[0].Filename != "movie.part2.rar" {
		t.Errorf("part filename = %q, want movie.part2.rar (from the URL)", res.Items[0].Filename)
	}

	if _, err := mgr.AddToPackage("missing", []LinkInput{{URL: "https://x.example.com/a"}}, AddOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown package: err = %v, want ErrNotFound", err)
	}
}
