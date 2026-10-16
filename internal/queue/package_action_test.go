package queue

import (
	"errors"
	"testing"
)

func TestApplyPackageAction(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{})
	pkg, items, err := mgr.AddPackage("p", []string{"http://example.invalid/a", "http://example.invalid/b"}, AddOptions{StartPaused: true})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := mgr.AddLink("http://example.invalid/other", AddOptions{StartPaused: true})

	n, err := mgr.ApplyPackageAction(pkg.ID, PackageActionResume)
	if err != nil || n != 2 {
		t.Fatalf("resume: n=%d err=%v", n, err)
	}
	for _, it := range items {
		got, _ := mgr.Get(it.ID)
		if got.State != StateQueued {
			t.Errorf("item %s state = %s, want queued", it.ID, got.State)
		}
	}
	if got, _ := mgr.Get(other.ID); got.State != StatePaused {
		t.Errorf("item outside the package was touched: %s", got.State)
	}

	if n, err := mgr.ApplyPackageAction(pkg.ID, PackageActionPause); err != nil || n != 2 {
		t.Fatalf("pause: n=%d err=%v", n, err)
	}
	if n, _ := mgr.ApplyPackageAction(pkg.ID, PackageActionRetry); n != 0 {
		t.Errorf("retry on non-failed items changed %d", n)
	}
	if _, err := mgr.ApplyPackageAction(pkg.ID, "bogus"); !errors.Is(err, ErrInvalidAction) {
		t.Errorf("bogus action err = %v", err)
	}

	if n, err := mgr.ApplyPackageAction(pkg.ID, PackageActionDelete); err != nil || n != 2 {
		t.Fatalf("delete: n=%d err=%v", n, err)
	}
	for _, p := range mgr.ListPackages() {
		if p.ID == pkg.ID {
			t.Error("package still listed after deleting all its items")
		}
	}
	if _, err := mgr.ApplyPackageAction(pkg.ID, PackageActionPause); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRenamePackage(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{})
	pkg, _, _ := mgr.AddPackage("old", []string{"http://example.invalid/a"}, AddOptions{StartPaused: true})
	got, err := mgr.RenamePackage(pkg.ID, "new")
	if err != nil || got.Name != "new" {
		t.Fatalf("rename: %+v %v", got, err)
	}
	if _, err := mgr.RenamePackage("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestAddLinks_DefaultPriorityIsNormal(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{})
	res, err := mgr.AddLinks([]LinkInput{{URL: "http://h.example/x.part1.rar"}}, "", AddOptions{StartPaused: true})
	if err != nil {
		t.Fatal(err)
	}
	if p := res.Items[0].Priority; p != PriorityNormal {
		t.Fatalf("priority = %s, want normal for an add without explicit priority", p)
	}
}
