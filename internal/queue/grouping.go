package queue

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/simuuh/hopper-downloader/internal/extract"
	"github.com/simuuh/hopper-downloader/plugins"
)

// LinkInput is one link handed to AddLinks, optionally with metadata the
// caller already knows (a DLC container's per-file name/size, say).
type LinkInput struct {
	URL      string
	Filename string
	Password string
	Size     int64
}

// AddResult reports what AddLinks did.
type AddResult struct {
	// Packages lists every package that received at least one new item —
	// newly created ones as well as existing ones new parts were merged
	// into (see AddLinks).
	Packages []*Package `json:"packages"`
	Items    []*Item    `json:"items"`
	// Duplicates lists input URLs that were skipped because the same URL,
	// or a file with the same name in the same package (another mirror of
	// a part that's already queued), is already in the queue.
	Duplicates []string `json:"duplicates"`
}

// ErrNoLinks is returned by AddLinks when none of the input URLs were
// valid http(s) URLs.
var ErrNoLinks = errors.New("queue: no valid links")

// AddLinks is the "smart" add path used by Click'n'Load, DLC/link-list
// imports and multi-link pastes in the Web UI. Unlike AddPackage (which
// puts every URL into one new package as given) it:
//
//   - expands container links (plugins.Expander, e.g. hide.cx) first, so
//     their parts take part in the grouping below like any other link;
//   - skips duplicates: a URL that's already queued, or a filename that
//     already exists in the target package (a mirror of the same part);
//   - groups archive parts by release — every "movie.partN.rar" (or
//     .rar/.rNN, .zip, ...) sharing one extract.GroupBase lands in one
//     package, so a submission containing several releases is split into
//     one package each;
//   - merges into an existing package that already holds parts of the
//     same release, instead of creating a second package for it. This is
//     what makes "parts 1–3 via one Click'n'Load, parts 4–6 via another"
//     (or several DLCs of the same release) end up together — required
//     for auto-extraction, which only looks for sibling parts within one
//     package.
//
// name is the caller's preferred package name (DLC package name, the CnL
// "package" field, ...) and is used when the input forms a single group;
// otherwise each release package is named after its GroupBase. Links that
// aren't recognizable archive parts are collected into one package named
// name (or added package-less when it's just a single link and no name was
// given, same as AddLink).
func (m *Manager) AddLinks(links []LinkInput, name string, opts AddOptions) (*AddResult, error) {
	entries := m.expandLinks(links)
	if len(entries) == 0 {
		return nil, ErrNoLinks
	}

	// Serialize the dedupe/group/merge decision so two concurrent
	// submissions of the same release (e.g. a double-clicked CnL button)
	// can't both decide to create a new package for it.
	m.addMu.Lock()
	defer m.addMu.Unlock()

	res := &AddResult{Duplicates: []string{}}
	entries = m.dropDuplicateURLs(entries, res)

	type group struct {
		key     string // lowercased GroupBase, "" for non-archives
		base    string
		entries []LinkInput
	}
	var order []string
	groups := map[string]*group{}
	for _, e := range entries {
		key, base := "", ""
		if fn := groupingName(e); fn != "" && extract.IsArchive(fn) {
			base = extract.GroupBase(fn)
			key = strings.ToLower(base)
			if e.Filename == "" {
				// Knowing the real archive name up front is what lets
				// sequential_parts ordering and extraction grouping work
				// before the download starts; a hoster URL like
				// rapidgator.net/file/<id>/movie.part1.rar.html carries it.
				e.Filename = fn
			}
		}
		g, ok := groups[key]
		if !ok {
			g = &group{key: key, base: base}
			groups[key] = g
			order = append(order, key)
		}
		g.entries = append(g.entries, e)
	}

	archiveGroups := 0
	for _, k := range order {
		if k != "" {
			archiveGroups++
		}
	}

	pkgSeen := map[string]bool{}
	var firstErr error
	for _, k := range order {
		g := groups[k]
		sort.SliceStable(g.entries, func(i, j int) bool { return partLess(g.entries[i], g.entries[j]) })

		var pkgID string
		switch {
		case g.key != "":
			pkgID = m.findPackageForGroup(g.key)
			if pkgID != "" {
				m.setPackagePasswordIfEmpty(pkgID, opts.Password)
			} else {
				pkgName := g.base
				if archiveGroups == 1 && strings.TrimSpace(name) != "" {
					pkgName = name
				}
				p, err := m.newPackage(pkgName, opts.Password)
				if err != nil {
					return nil, err
				}
				pkgID = p.ID
			}
		case len(g.entries) > 1 || strings.TrimSpace(name) != "":
			pkgName := strings.TrimSpace(name)
			if pkgName == "" {
				pkgName = "Links " + time.Now().Format("2006-01-02 15:04")
			}
			p, err := m.newPackage(pkgName, opts.Password)
			if err != nil {
				return nil, err
			}
			pkgID = p.ID
		}

		existing := m.filenamesInPackage(pkgID)
		var batch []*Item
		for _, e := range g.entries {
			if pkgID != "" && e.Filename != "" && existing[strings.ToLower(e.Filename)] {
				res.Duplicates = append(res.Duplicates, e.URL)
				continue
			}
			itemOpts := opts
			itemOpts.Filenames = nil
			itemOpts.Filename = e.Filename
			itemOpts.Size = e.Size
			if e.Password != "" {
				itemOpts.Password = e.Password
			}
			it, err := m.newItem(pkgID, e.URL, itemOpts)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if e.Filename != "" {
				existing[strings.ToLower(e.Filename)] = true
			}
			batch = append(batch, it)
		}
		// One transaction per group instead of one per link.
		if len(batch) > 0 {
			added, err := m.insertItems(batch)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
			} else {
				res.Items = append(res.Items, added...)
				if pkgID != "" && !pkgSeen[pkgID] {
					pkgSeen[pkgID] = true
					if p := m.packageSnapshot(pkgID); p != nil {
						res.Packages = append(res.Packages, p)
					}
				}
			}
		}
		if pkgID != "" && !pkgSeen[pkgID] {
			// Nothing new went in (all duplicates/invalid): don't leave a
			// package we just created behind empty.
			m.removePackageIfEmpty(pkgID)
		}
	}

	if len(res.Items) == 0 && len(res.Duplicates) == 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrNoLinks, firstErr)
		}
		return nil, ErrNoLinks
	}
	return res, nil
}

// expandLinks validates links and replaces every container link a
// plugins.Expander claims with the links it expands to. Done before
// AddLinks takes addMu: expansion makes network calls.
func (m *Manager) expandLinks(links []LinkInput) []LinkInput {
	out := make([]LinkInput, 0, len(links))
	for _, l := range links {
		l.URL = strings.TrimSpace(l.URL)
		if validHTTPURL(l.URL) != nil {
			continue
		}
		if hoster, ok := m.matchHoster(l.URL); ok {
			if exp, ok := hoster.(plugins.Expander); ok {
				if expanded, err := exp.Expand(l.URL); err == nil && len(expanded) > 0 {
					for _, x := range expanded {
						pw := x.Password
						if pw == "" {
							pw = l.Password
						}
						out = append(out, LinkInput{URL: x.URL, Filename: x.Name, Size: x.Size, Password: pw})
					}
					continue
				}
			}
		}
		out = append(out, l)
	}
	return out
}

// dropDuplicateURLs removes entries whose URL is already queued, or that
// repeat an earlier entry of the same submission, recording them in res.
func (m *Manager) dropDuplicateURLs(entries []LinkInput, res *AddResult) []LinkInput {
	m.mu.RLock()
	known := make(map[string]bool, len(m.items))
	for _, it := range m.items {
		known[it.URL] = true
	}
	m.mu.RUnlock()

	out := entries[:0:0]
	for _, e := range entries {
		if known[e.URL] {
			res.Duplicates = append(res.Duplicates, e.URL)
			continue
		}
		known[e.URL] = true
		out = append(out, e)
	}
	return out
}

// groupingName is the filename used to recognize e as an archive part:
// the caller-supplied one, else the name embedded in the URL.
func groupingName(e LinkInput) string {
	if e.Filename != "" {
		return e.Filename
	}
	return plugins.FilenameFromURL(e.URL)
}

// partLess orders archive parts numerically (part2 before part10).
func partLess(a, b LinkInput) bool {
	pa, oka := extract.PartNumber(a.Filename)
	pb, okb := extract.PartNumber(b.Filename)
	if oka && okb {
		return pa < pb
	}
	return false
}

// findPackageForGroup returns the ID of the most recently created
// package that already contains an item of archive group key (lowercased
// extract.GroupBase), or "" if none does.
func (m *Manager) findPackageForGroup(key string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var best *Package
	for _, it := range m.items {
		if it.PackageID == "" || it.Filename == "" || !extract.IsArchive(it.Filename) {
			continue
		}
		if strings.ToLower(extract.GroupBase(it.Filename)) != key {
			continue
		}
		p, ok := m.packages[it.PackageID]
		if !ok {
			continue
		}
		if best == nil || p.CreatedAt.After(best.CreatedAt) {
			best = p
		}
	}
	if best == nil {
		return ""
	}
	return best.ID
}

// filenamesInPackage returns the lowercased filenames of every item in
// package id.
func (m *Manager) filenamesInPackage(id string) map[string]bool {
	out := map[string]bool{}
	if id == "" {
		return out
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, it := range m.items {
		if it.PackageID == id && it.Filename != "" {
			out[strings.ToLower(it.Filename)] = true
		}
	}
	return out
}

// newPackage creates, persists and publishes an empty package.
func (m *Manager) newPackage(name, password string) (*Package, error) {
	pkg := &Package{ID: newID(), Name: name, Password: password, CreatedAt: time.Now()}
	if err := m.store.PutPackage(pkg); err != nil {
		return nil, fmt.Errorf("queue: persist package: %w", err)
	}
	m.mu.Lock()
	m.packages[pkg.ID] = pkg
	cp := *pkg
	m.mu.Unlock()
	m.publish(Event{Type: EventPackageUpdated, Package: &cp})
	return &cp, nil
}

// packageSnapshot returns a copy of package id, or nil.
func (m *Manager) packageSnapshot(id string) *Package {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.packages[id]
	if !ok {
		return nil
	}
	cp := *p
	return &cp
}

// setPackagePasswordIfEmpty gives package id password if it has none yet
// — a later submission of the same release may be the first to carry the
// archive password (Click'n'Load "passwords" field, DLC metadata).
func (m *Manager) setPackagePasswordIfEmpty(id, password string) {
	if password == "" {
		return
	}
	m.mu.Lock()
	p, ok := m.packages[id]
	if !ok || p.Password != "" {
		m.mu.Unlock()
		return
	}
	p.Password = password
	cp := *p
	if err := m.store.PutPackage(&cp); err != nil {
		m.log.Error("queue: persist package", "package_id", id, "error", err)
	}
	m.mu.Unlock()
	m.publish(Event{Type: EventPackageUpdated, Package: &cp})
}
