package queue

import (
	"errors"
	"testing"

	"github.com/simuuh/hopper-downloader/plugins"
)

// expandingHoster is a fakeHoster that also implements plugins.Expander,
// for testing queue.Manager's Expander wiring without a real multi-part
// plugin like hidecx.
type expandingHoster struct {
	fakeHoster
	links     []plugins.ExpandedLink
	expandErr error
}

func (e *expandingHoster) Expand(string) ([]plugins.ExpandedLink, error) {
	if e.expandErr != nil {
		return nil, e.expandErr
	}
	return e.links, nil
}

func packageByID(mgr *Manager, id string) (*Package, bool) {
	for _, p := range mgr.ListPackages() {
		if p.ID == id {
			return p, true
		}
	}
	return nil, false
}

func itemsInPackage(mgr *Manager, pkgID string) []*Item {
	var out []*Item
	for _, it := range mgr.List() {
		if it.PackageID == pkgID {
			out = append(out, it)
		}
	}
	return out
}

func TestAddItem_ExpandsIntoOneNewPackage(t *testing.T) {
	links := []plugins.ExpandedLink{
		{URL: "https://real-hoster.example/part1.rar", Name: "movie.part1.rar", Size: 100, Password: "secret"},
		{URL: "https://real-hoster.example/part2.rar", Name: "movie.part2.rar", Size: 200, Password: "secret"},
	}
	expander := &expandingHoster{
		fakeHoster: fakeHoster{name: "container-plugin", matchHosts: []string{"container.example"}},
		links:      links,
	}
	realHoster := &passthroughHoster{name: "real-hoster"}

	reg := plugins.NewRegistry(realHoster, expander)
	mgr := newResolveTestManager(t, reg, &fakeAccountLookup{})

	it, err := mgr.AddLink("https://container.example/x", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}

	if it.URL != links[0].URL {
		t.Errorf("URL = %q, want the expanded (real hoster) URL %q", it.URL, links[0].URL)
	}
	if it.HosterKey != "real-hoster" {
		t.Errorf("HosterKey = %q, want real-hoster (re-matched on the expanded URL, not container-plugin)", it.HosterKey)
	}
	if it.Filename != "movie.part1.rar" {
		t.Errorf("Filename = %q", it.Filename)
	}
	if it.Size != 100 {
		t.Errorf("Size = %d, want 100", it.Size)
	}
	if it.PackageID == "" {
		t.Fatal("expected item to belong to a package")
	}

	siblings := itemsInPackage(mgr, it.PackageID)
	if len(siblings) != 2 {
		t.Fatalf("items in package = %d, want 2", len(siblings))
	}
	for _, s := range siblings {
		if s.HosterKey != "real-hoster" {
			t.Errorf("sibling HosterKey = %q, want real-hoster", s.HosterKey)
		}
	}

	pkg, ok := packageByID(mgr, it.PackageID)
	if !ok {
		t.Fatal("package not found")
	}
	if pkg.Password != "secret" {
		t.Errorf("Package.Password = %q, want secret", pkg.Password)
	}
}

func TestAddItem_ExpansionErrorFallsBackToSingleItem(t *testing.T) {
	expander := &expandingHoster{
		fakeHoster: fakeHoster{name: "container-plugin", matchHosts: []string{"container.example"}},
		expandErr:  errors.New("boom"),
	}
	reg := plugins.NewRegistry(expander)
	mgr := newResolveTestManager(t, reg, &fakeAccountLookup{})

	const rawURL = "https://container.example/x"
	it, err := mgr.AddLink(rawURL, AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	if it.URL != rawURL {
		t.Errorf("URL = %q, want the original container URL (expansion failed, single-item fallback)", it.URL)
	}
	if it.HosterKey != "container-plugin" {
		t.Errorf("HosterKey = %q, want container-plugin", it.HosterKey)
	}
	if it.PackageID != "" {
		t.Errorf("PackageID = %q, want empty (no package for a plain AddLink item)", it.PackageID)
	}
}

func TestAddItem_ExpansionEmptyFallsBackToSingleItem(t *testing.T) {
	expander := &expandingHoster{
		fakeHoster: fakeHoster{name: "container-plugin", matchHosts: []string{"container.example"}},
		links:      nil,
	}
	reg := plugins.NewRegistry(expander)
	mgr := newResolveTestManager(t, reg, &fakeAccountLookup{})

	const rawURL = "https://container.example/x"
	it, err := mgr.AddLink(rawURL, AddOptions{})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	if it.URL != rawURL {
		t.Errorf("URL = %q, want the original container URL", it.URL)
	}
}

func TestAddItem_ExplicitHosterKeyBypassesExpansion(t *testing.T) {
	links := []plugins.ExpandedLink{
		{URL: "https://real-hoster.example/part1.rar", Name: "movie.part1.rar"},
	}
	expander := &expandingHoster{
		fakeHoster: fakeHoster{name: "container-plugin", matchHosts: []string{"container.example"}},
		links:      links,
	}
	reg := plugins.NewRegistry(expander)
	mgr := newResolveTestManager(t, reg, &fakeAccountLookup{})

	const rawURL = "https://container.example/x"
	it, err := mgr.AddLink(rawURL, AddOptions{HosterKey: "manual-override"})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	if it.URL != rawURL {
		t.Errorf("URL = %q, want the original URL (explicit HosterKey bypasses expansion)", it.URL)
	}
	if it.HosterKey != "manual-override" {
		t.Errorf("HosterKey = %q, want manual-override", it.HosterKey)
	}
}

func TestAddPackage_ExpandedURLJoinsExistingPackageAndKeepsExplicitPassword(t *testing.T) {
	links := []plugins.ExpandedLink{
		{URL: "https://real-hoster.example/part1.rar", Name: "movie.part1.rar", Password: "container-password"},
		{URL: "https://real-hoster.example/part2.rar", Name: "movie.part2.rar", Password: "container-password"},
	}
	expander := &expandingHoster{
		fakeHoster: fakeHoster{name: "container-plugin", matchHosts: []string{"container.example"}},
		links:      links,
	}
	realHoster := &passthroughHoster{name: "real-hoster"}
	reg := plugins.NewRegistry(realHoster, expander)
	mgr := newResolveTestManager(t, reg, &fakeAccountLookup{})

	pkg, items, err := mgr.AddPackage("mypkg", []string{
		"https://real-hoster.example/other.txt",
		"https://container.example/x",
	}, AddOptions{Password: "explicit-password"})
	if err != nil {
		t.Fatalf("AddPackage: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3 (1 plain + 2 expanded)", len(items))
	}

	got, ok := packageByID(mgr, pkg.ID)
	if !ok {
		t.Fatal("package not found")
	}
	if got.Password != "explicit-password" {
		t.Errorf("Package.Password = %q, want explicit-password to survive (not overwritten by the container's password)", got.Password)
	}

	siblings := itemsInPackage(mgr, pkg.ID)
	if len(siblings) != 3 {
		t.Fatalf("items in package = %d, want 3", len(siblings))
	}
}

// TestAddPackage_UsesPerURLFilenamesWhenProvided is the regression test
// for a real gap found while building sequential-part ordering
// (Config.SequentialParts): AddPackage previously had no way for a
// caller who already knows every file's real name (a DLC container's
// own per-file "filename" field, in particular — see
// api/import_handlers.go) to supply it up front, so a multi-part RAR
// imported that way could never be recognized as one until its
// download actually started, by which point sequential ordering has
// nothing left to decide.
func TestAddPackage_UsesPerURLFilenamesWhenProvided(t *testing.T) {
	reg := plugins.NewRegistry(&passthroughHoster{name: "direct"})
	mgr := newResolveTestManager(t, reg, nil)

	urls := []string{"https://example.com/a", "https://example.com/b", "https://example.com/c"}
	filenames := []string{"movie.part1.rar", "movie.part2.rar", ""}

	_, items, err := mgr.AddPackage("movie", urls, AddOptions{Filenames: filenames})
	if err != nil {
		t.Fatalf("AddPackage: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	got := make(map[string]string, len(items))
	for _, it := range items {
		got[it.URL] = it.Filename
	}
	if got[urls[0]] != "movie.part1.rar" {
		t.Errorf("Filename for %s = %q, want movie.part1.rar", urls[0], got[urls[0]])
	}
	if got[urls[1]] != "movie.part2.rar" {
		t.Errorf("Filename for %s = %q, want movie.part2.rar", urls[1], got[urls[1]])
	}
	if got[urls[2]] != "" {
		t.Errorf("Filename for %s = %q, want empty (no filename provided for this URL)", urls[2], got[urls[2]])
	}
}

func TestAddPackage_FilenamesShorterThanURLsLeavesRestUnset(t *testing.T) {
	reg := plugins.NewRegistry(&passthroughHoster{name: "direct"})
	mgr := newResolveTestManager(t, reg, nil)

	urls := []string{"https://example.com/a", "https://example.com/b"}
	filenames := []string{"only-first.rar"}

	_, items, err := mgr.AddPackage("pkg", urls, AddOptions{Filenames: filenames})
	if err != nil {
		t.Fatalf("AddPackage: %v", err)
	}
	got := make(map[string]string, len(items))
	for _, it := range items {
		got[it.URL] = it.Filename
	}
	if got[urls[0]] != "only-first.rar" {
		t.Errorf("Filename for %s = %q, want only-first.rar", urls[0], got[urls[0]])
	}
	if got[urls[1]] != "" {
		t.Errorf("Filename for %s = %q, want empty (Filenames slice too short to cover it)", urls[1], got[urls[1]])
	}
}

func TestPackageNameFromLinks(t *testing.T) {
	name := packageNameFromLinks([]plugins.ExpandedLink{
		{Name: "52e0dabfb9ede5f9.part1.rar"},
		{Name: "52e0dabfb9ede5f9.part2.rar"},
	})
	if name != "52e0dabfb9ede5f9" {
		t.Errorf("packageNameFromLinks = %q, want the shared base name", name)
	}

	if got := packageNameFromLinks(nil); got == "" {
		t.Error("packageNameFromLinks(nil) should return a non-empty fallback name")
	}
}
