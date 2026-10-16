package queue

import (
	"fmt"
	"strings"
	"time"

	"github.com/simuuh/hopper-downloader/internal/extract"
	"github.com/simuuh/hopper-downloader/internal/notify"
)

// PackageNotifier is told whenever a package finishes (see
// Manager.checkFinished). *notify.Notifier satisfies it; it must not
// block — delivery happens in the background.
type PackageNotifier interface {
	PackageFinished(ev notify.Event)
}

// checkFinished fires Config.Notifier once the package that itemID belongs
// to has finished: every item is done or failed, and no extraction for it
// is still pending or running. An item without a package counts as a
// package of one.
//
// It is called from the three places a package can become finished:
// complete() (after maybeExtract has had the chance to claim the group),
// a final fail(), and the end of runExtraction. Because those can race and
// a package can finish more than once (a failed item retried later, more
// parts merged in, a re-extract), each package remembers the outcome it
// last notified about and only notifies again when that outcome changes.
func (m *Manager) checkFinished(itemID string) {
	if m.cfg.Notifier == nil {
		return
	}

	m.mu.RLock()
	it, ok := m.items[itemID]
	if !ok {
		m.mu.RUnlock()
		return
	}
	pkgID := it.PackageID
	var members []Item
	if pkgID == "" {
		members = []Item{*it}
	} else {
		for _, sib := range m.items {
			if sib.PackageID == pkgID {
				members = append(members, *sib)
			}
		}
	}
	var pkg Package
	if p, ok := m.packages[pkgID]; ok {
		pkg = *p
	}
	m.mu.RUnlock()

	ev := notify.Event{Event: notify.EventPackageFinished, PackageID: pkgID, Package: pkg.Name, Items: len(members)}
	for _, mem := range members {
		switch mem.State {
		case StateDone:
			ev.Done++
			ev.Bytes += mem.Size
		case StateError:
			ev.Failed++
		default:
			return // still queued, downloading or paused
		}
		switch mem.ExtractState {
		case ExtractStateExtracting:
			return
		case ExtractStateExtractFailed:
			ev.Extract = string(ExtractStateExtractFailed)
		case ExtractStateExtracted:
			if ev.Extract == "" {
				ev.Extract = string(ExtractStateExtracted)
			}
		}
	}
	if m.extractionPending(pkgID) || m.extractionDue(members) {
		return
	}

	switch {
	case ev.Done == 0:
		ev.Status = notify.StatusFailed
	case ev.Failed > 0 || ev.Extract == string(ExtractStateExtractFailed):
		ev.Status = notify.StatusPartial
	default:
		ev.Status = notify.StatusCompleted
	}
	if pkgID == "" {
		ev.Package = it.Filename
	} else {
		ev.Path = packageDir(&pkg)
	}
	if ev.Package == "" {
		ev.Package = it.URL
	}

	key := pkgID
	if key == "" {
		key = "item:" + itemID
	}
	sig := fmt.Sprintf("%d/%d/%d/%s", ev.Items, ev.Done, ev.Failed, ev.Extract)
	m.finishedMu.Lock()
	if m.finishedSig[key] == sig {
		m.finishedMu.Unlock()
		return
	}
	m.finishedSig[key] = sig
	m.finishedMu.Unlock()

	ev.FinishedAt = time.Now()
	m.log.Info("queue: package finished", "package_id", pkgID, "package", ev.Package, "status", ev.Status)
	m.cfg.Notifier.PackageFinished(ev)
}

// extractionPending reports whether maybeExtract has claimed an archive
// group of pkgID whose runExtraction hasn't finished yet (it may still be
// waiting for parts to show up on disk, before ExtractStateExtracting is
// even set).
func (m *Manager) extractionPending(pkgID string) bool {
	prefix := pkgID + "|"
	m.extractTriggeredMu.Lock()
	defer m.extractTriggeredMu.Unlock()
	for k := range m.extractTriggered {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// extractionDue reports whether members contain an archive group that
// maybeExtract will extract but hasn't produced a result for yet: every
// part done, a first part present, extraction enabled, and no
// extracted/extract_failed state. Decided from item state rather than
// from extractTriggered alone, because two parts completing at the same
// time leave a window in which the last part is already done but its
// maybeExtract hasn't claimed the group yet — a sibling's checkFinished
// running in that window would otherwise report the package finished
// before extraction. Known limit: an archive that finished while
// auto-extract was off counts as due once it's switched on, so its
// package won't notify until it's extracted (re-extract button).
func (m *Manager) extractionDue(members []Item) bool {
	if m.extractor == nil || m.extractSettings == nil || !m.extractSettings.ExtractEnabled() {
		return false
	}
	type group struct{ allDone, hasFirst, hasResult bool }
	groups := map[string]*group{}
	for _, mem := range members {
		if mem.Filename == "" || !extract.IsArchive(mem.Filename) {
			continue
		}
		key := extract.GroupBase(mem.Filename)
		g, ok := groups[key]
		if !ok {
			g = &group{allDone: true}
			groups[key] = g
		}
		if mem.State != StateDone {
			g.allDone = false
		}
		if extract.IsFirstPart(mem.Filename) {
			g.hasFirst = true
		}
		if mem.ExtractState == ExtractStateExtracted || mem.ExtractState == ExtractStateExtractFailed {
			g.hasResult = true
		}
	}
	for _, g := range groups {
		if g.allDone && g.hasFirst && !g.hasResult {
			return true
		}
	}
	return false
}

// forgetFinished drops the remembered notification outcome for a removed
// package or item, so the map doesn't grow forever.
func (m *Manager) forgetFinished(key string) {
	m.finishedMu.Lock()
	delete(m.finishedSig, key)
	m.finishedMu.Unlock()
}
