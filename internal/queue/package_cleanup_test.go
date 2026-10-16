package queue

import "testing"

func TestCancel_LastItemRemovesPackage(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1})

	pkg, items, err := mgr.AddPackage("pkg", []string{"http://example.invalid/a", "http://example.invalid/b"}, AddOptions{StartPaused: true})
	if err != nil {
		t.Fatalf("AddPackage: %v", err)
	}
	events, cancel := mgr.Subscribe()
	defer cancel()

	if err := mgr.Cancel(items[0].ID); err != nil {
		t.Fatal(err)
	}
	if len(mgr.ListPackages()) != 1 {
		t.Fatalf("package removed while it still had an item")
	}
	if err := mgr.Cancel(items[1].ID); err != nil {
		t.Fatal(err)
	}
	if n := len(mgr.ListPackages()); n != 0 {
		t.Fatalf("packages = %d after last item cancelled, want 0", n)
	}
	persisted, err := store.ListPackages()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 0 {
		t.Fatalf("persisted packages = %d, want 0", len(persisted))
	}

	sawRemoved := false
	for len(events) > 0 {
		ev := <-events
		if ev.Type == EventPackageRemoved && ev.Package != nil && ev.Package.ID == pkg.ID {
			sawRemoved = true
		}
	}
	if !sawRemoved {
		t.Error("no package_removed event published")
	}
}

func TestNew_DropsOrphanPackages(t *testing.T) {
	store := newTestStore(t)
	if err := store.PutPackage(&Package{ID: "orphan", Name: "old"}); err != nil {
		t.Fatal(err)
	}
	mgr, _ := newTestManager(t, store, Config{})
	if n := len(mgr.ListPackages()); n != 0 {
		t.Fatalf("packages = %d, want orphan dropped", n)
	}
	persisted, _ := store.ListPackages()
	if len(persisted) != 0 {
		t.Fatalf("orphan still persisted")
	}
}

func TestAddPackage_AllInvalidLeavesNoPackage(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{})
	if _, _, err := mgr.AddPackage("bad", []string{"not a url"}, AddOptions{}); err == nil {
		t.Fatal("expected error")
	}
	if n := len(mgr.ListPackages()); n != 0 {
		t.Fatalf("packages = %d, want 0", n)
	}
}
