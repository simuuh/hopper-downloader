package queue

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/simuuh/hopper-downloader/internal/extract"
	"github.com/simuuh/hopper-downloader/internal/pathsafe"
)

// ErrItemNotEditable is returned by EditItem for an item that is
// downloading or already done — its URL/filename are in use or already
// on disk.
var ErrItemNotEditable = errors.New("queue: only queued, paused or failed items can be edited")

// ErrDuplicateURL is returned by EditItem when the new URL is already
// queued as another item.
var ErrDuplicateURL = errors.New("queue: this URL is already in the queue")

// ItemEdit lists the fields EditItem changes; nil leaves a field as is.
type ItemEdit struct {
	// URL replaces the link, e.g. with a working mirror of a dead one.
	URL *string
	// Filename overrides the target filename; "" clears the override (the
	// hoster-reported name is used again).
	Filename *string
}

// EditItem changes an item's URL and/or filename. Only items that aren't
// downloading or done can be edited. A new URL re-matches the hoster
// plugin and gives a failed item a fresh start (state queued, automatic
// retry budget and error cleared) — replacing a dead link is the main
// reason to edit one.
func (m *Manager) EditItem(id string, e ItemEdit) (*Item, error) {
	var newURL, hosterKey string
	if e.URL != nil {
		newURL = strings.TrimSpace(*e.URL)
		if err := validHTTPURL(newURL); err != nil {
			return nil, err
		}
		// Matched before taking m.mu: a plugin's Match may make a network
		// call (Real-Debrid's host list).
		if h, ok := m.matchHoster(newURL); ok {
			hosterKey = h.Name()
		}
	}
	var newName string
	if e.Filename != nil {
		if n := strings.TrimSpace(*e.Filename); n != "" {
			newName = pathsafe.SanitizeFilename(n)
		}
	}

	m.mu.Lock()
	it, ok := m.items[id]
	if !ok {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	switch it.State {
	case StateQueued, StatePaused, StateError:
	default:
		m.mu.Unlock()
		return nil, ErrItemNotEditable
	}
	if e.URL != nil && newURL != it.URL {
		for _, other := range m.items {
			if other.ID != id && other.URL == newURL {
				m.mu.Unlock()
				return nil, ErrDuplicateURL
			}
		}
		it.URL = newURL
		it.HosterKey = hosterKey
		it.Error = ""
		it.Attempts = 0
		it.RetryAfter = nil
		it.CaptchaURL = ""
		if it.State == StateError {
			it.State = StateQueued
		}
	}
	if e.Filename != nil {
		it.Filename = newName
	}
	it.UpdatedAt = time.Now()
	if err := m.store.PutItem(it); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("queue: persist item: %w", err)
	}
	cp := *it
	m.mu.Unlock()

	m.publish(Event{Type: EventItemUpdated, Item: &cp})
	m.wakeAsync()
	return &cp, nil
}

// AddToPackage adds links to the existing package id — the Web UI's "add
// links" on a package, e.g. a missing part or a replacement mirror.
// Container links are expanded and duplicates skipped exactly like
// AddLinks does, but every new item goes into package id instead of being
// grouped by release.
func (m *Manager) AddToPackage(id string, links []LinkInput, opts AddOptions) (*AddResult, error) {
	if m.packageSnapshot(id) == nil {
		return nil, ErrNotFound
	}
	entries := m.expandLinks(links)
	if len(entries) == 0 {
		return nil, ErrNoLinks
	}

	m.addMu.Lock()
	defer m.addMu.Unlock()

	res := &AddResult{Duplicates: []string{}}
	entries = m.dropDuplicateURLs(entries, res)
	existing := m.filenamesInPackage(id)
	var firstErr error
	for _, e := range entries {
		if e.Filename == "" {
			if fn := groupingName(e); fn != "" && extract.IsArchive(fn) {
				e.Filename = fn
			}
		}
		if e.Filename != "" && existing[strings.ToLower(e.Filename)] {
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
		it, err := m.addPlainItem(id, e.URL, itemOpts)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if e.Filename != "" {
			existing[strings.ToLower(e.Filename)] = true
		}
		res.Items = append(res.Items, it)
	}
	if len(res.Items) == 0 && len(res.Duplicates) == 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrNoLinks, firstErr)
		}
		return nil, ErrNoLinks
	}
	if p := m.packageSnapshot(id); p != nil {
		res.Packages = []*Package{p}
	}
	return res, nil
}
