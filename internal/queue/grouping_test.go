package queue

import (
	"errors"
	"sort"
	"testing"

	"github.com/simuuh/hopper-downloader/internal/downloader"
	"github.com/simuuh/hopper-downloader/internal/scheduler"
	"github.com/simuuh/hopper-downloader/plugins"
	"github.com/simuuh/hopper-downloader/plugins/directhttp"
)

func rg(name string) LinkInput {
	return LinkInput{URL: "https://rapidgator.net/file/" + name + "-id/" + name + ".html"}
}

func itemsByPackage(mgr *Manager) map[string][]string {
	out := map[string][]string{}
	for _, it := range mgr.List() {
		out[it.PackageID] = append(out[it.PackageID], it.Filename)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func TestAddLinks_SplitsReleasesIntoPackages(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{})
	res, err := mgr.AddLinks([]LinkInput{
		rg("movie.a.part1.rar"), rg("show.s01e01.part1.rar"), rg("movie.a.part2.rar"), rg("show.s01e01.part2.rar"),
	}, "", AddOptions{StartPaused: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Packages) != 2 || len(res.Items) != 4 {
		t.Fatalf("packages=%d items=%d, want 2/4", len(res.Packages), len(res.Items))
	}
	names := map[string]bool{}
	for _, p := range res.Packages {
		names[p.Name] = true
	}
	if !names["movie.a"] || !names["show.s01e01"] {
		t.Fatalf("package names = %v", names)
	}
	for _, it := range res.Items {
		if it.Filename == "" {
			t.Errorf("item %s has no filename derived from its URL", it.URL)
		}
	}
}

func TestAddLinks_MergesLaterPartsIntoExistingPackage(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{})
	first, err := mgr.AddLinks([]LinkInput{rg("movie.part1.rar"), rg("movie.part2.rar")}, "Movie 2026 (DLC name)", AddOptions{StartPaused: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := mgr.AddLinks([]LinkInput{rg("movie.part3.rar"), rg("movie.part4.rar")}, "", AddOptions{StartPaused: true, Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Packages[0].Name != "Movie 2026 (DLC name)" {
		t.Errorf("single-release name = %q, want caller-supplied name", first.Packages[0].Name)
	}
	if second.Packages[0].ID != first.Packages[0].ID {
		t.Fatalf("later parts went to a new package")
	}
	if got := len(itemsByPackage(mgr)[first.Packages[0].ID]); got != 4 {
		t.Fatalf("package has %d items, want 4", got)
	}
	if second.Packages[0].Password != "secret" {
		t.Errorf("merged package password = %q, want inherited %q", second.Packages[0].Password, "secret")
	}
	if len(mgr.ListPackages()) != 1 {
		t.Errorf("packages = %d, want 1", len(mgr.ListPackages()))
	}
}

func TestAddLinks_SkipsDuplicateURLsAndMirrors(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{})
	if _, err := mgr.AddLinks([]LinkInput{rg("movie.part1.rar"), rg("movie.part2.rar")}, "", AddOptions{StartPaused: true}); err != nil {
		t.Fatal(err)
	}
	mirror := LinkInput{URL: "https://other.example/dl/123", Filename: "movie.part2.rar"}
	res, err := mgr.AddLinks([]LinkInput{rg("movie.part1.rar"), mirror, rg("movie.part3.rar"), rg("movie.part3.rar")}, "", AddOptions{StartPaused: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Filename != "movie.part3.rar" {
		t.Fatalf("added = %+v, want only part3", res.Items)
	}
	if len(res.Duplicates) != 3 {
		t.Fatalf("duplicates = %v, want 3 (same URL, mirror, repeated in input)", res.Duplicates)
	}
}

func TestAddLinks_AllDuplicatesIsNotAnError(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{})
	in := []LinkInput{rg("a.part1.rar"), rg("a.part2.rar")}
	if _, err := mgr.AddLinks(in, "", AddOptions{StartPaused: true}); err != nil {
		t.Fatal(err)
	}
	res, err := mgr.AddLinks(in, "", AddOptions{StartPaused: true})
	if err != nil {
		t.Fatalf("all-duplicate submission: %v", err)
	}
	if len(res.Items) != 0 || len(res.Duplicates) != 2 {
		t.Fatalf("items=%d dups=%d", len(res.Items), len(res.Duplicates))
	}
	if len(mgr.ListPackages()) != 1 {
		t.Fatalf("empty package left behind: %d packages", len(mgr.ListPackages()))
	}
}

func TestAddLinks_PlainLinks(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{})
	res, err := mgr.AddLinks([]LinkInput{{URL: "https://example.com/file.iso"}}, "", AddOptions{StartPaused: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Items[0].PackageID != "" {
		t.Error("single plain link without a name should stay package-less like AddLink")
	}
	res, err = mgr.AddLinks([]LinkInput{{URL: "https://example.com/a.iso"}, {URL: "https://example.com/b.iso"}}, "isos", AddOptions{StartPaused: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Packages) != 1 || res.Packages[0].Name != "isos" {
		t.Fatalf("packages = %+v", res.Packages)
	}
}

func TestAddLinks_NoValidLinks(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{})
	if _, err := mgr.AddLinks([]LinkInput{{URL: "javascript:alert(1)"}}, "", AddOptions{}); !errors.Is(err, ErrNoLinks) {
		t.Fatalf("err = %v, want ErrNoLinks", err)
	}
}

type fakeExpander struct{ links []plugins.ExpandedLink }

func (f *fakeExpander) Name() string      { return "fakeexp" }
func (f *fakeExpander) Domains() []string { return []string{"container.example"} }
func (f *fakeExpander) Match(u string) bool {
	return len(u) > 26 && u[:26] == "https://container.example/"
}
func (f *fakeExpander) Resolve(string, *plugins.Account) (*plugins.FileInfo, error) {
	return nil, errors.New("unused")
}
func (f *fakeExpander) FetchDownloadURL(string, *plugins.Account) (string, error) {
	return "", errors.New("unused")
}
func (f *fakeExpander) Expand(string) ([]plugins.ExpandedLink, error) { return f.links, nil }

func TestAddLinks_ContainerPartsMergeWithDirectParts(t *testing.T) {
	exp := &fakeExpander{links: []plugins.ExpandedLink{
		{URL: "https://host.example/x2", Name: "rel.part2.rar"},
		{URL: "https://host.example/x3", Name: "rel.part3.rar"},
	}}
	reg := plugins.NewRegistry(directhttp.New(), exp)
	eng := downloader.New(downloader.Config{BaseDir: t.TempDir()})
	mgr, err := New(newTestStore(t), reg, eng, nil, nil, nil, nil, scheduler.New(), discardLogger(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AddLinks([]LinkInput{rg("rel.part1.rar")}, "", AddOptions{StartPaused: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AddLinks([]LinkInput{{URL: "https://container.example/abc"}}, "", AddOptions{StartPaused: true}); err != nil {
		t.Fatal(err)
	}
	byPkg := itemsByPackage(mgr)
	if len(byPkg) != 1 {
		t.Fatalf("groups = %v, want one package with all 3 parts", byPkg)
	}
	for _, names := range byPkg {
		if len(names) != 3 {
			t.Fatalf("names = %v", names)
		}
	}
}
