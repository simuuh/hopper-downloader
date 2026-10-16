package queue

import (
	"strings"

	"github.com/simuuh/hopper-downloader/internal/extract"
)

// queueOrder is the single definition of "top to bottom": the dispatcher
// starts items in exactly the order List returns them, which is the order
// the Web UI shows them in. Items are grouped like the UI groups them (a
// package, or a package-less item on its own):
//
//  1. groups by their priority — the highest priority among the group's
//     unfinished items — so raising one item's priority moves its whole
//     package up, as it is displayed;
//  2. then by the group's rank — the smallest Position of any of its
//     items, i.e. where the package was first queued; parts merged into
//     a package later still belong to that package's place;
//  3. within a group: item priority, then archive parts of the same set
//     by part number (part2 before part10, a later-merged part1 before
//     part3), then Position, then insertion time.
type queueOrder struct {
	rank map[string]int64
	prio map[string]Priority
}

// groupKey identifies an item's display group.
func groupKey(it *Item) string {
	if it.PackageID != "" {
		return it.PackageID
	}
	return "item:" + it.ID
}

// newQueueOrder precomputes the group ranks/priorities for items.
func newQueueOrder(items map[string]*Item) queueOrder {
	o := queueOrder{rank: make(map[string]int64), prio: make(map[string]Priority)}
	openPrio := make(map[string]Priority) // max priority of unfinished items
	allPrio := make(map[string]Priority)  // max priority of all items
	for _, it := range items {
		g := groupKey(it)
		if r, ok := o.rank[g]; !ok || it.Position < r {
			o.rank[g] = it.Position
		}
		if p, ok := allPrio[g]; !ok || it.Priority > p {
			allPrio[g] = it.Priority
		}
		if it.State != StateDone {
			if p, ok := openPrio[g]; !ok || it.Priority > p {
				openPrio[g] = it.Priority
			}
		}
	}
	for g, p := range allPrio {
		if op, ok := openPrio[g]; ok {
			p = op
		}
		o.prio[g] = p
	}
	return o
}

func (o queueOrder) less(a, b *Item) bool {
	ga, gb := groupKey(a), groupKey(b)
	if ga != gb {
		if pa, pb := o.prio[ga], o.prio[gb]; pa != pb {
			return pa > pb
		}
		if ra, rb := o.rank[ga], o.rank[gb]; ra != rb {
			return ra < rb
		}
		return ga < gb
	}
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	if sa, na, ok := partOf(a); ok {
		if sb, nb, ok := partOf(b); ok && sa == sb && na != nb {
			return na < nb
		}
	}
	if a.Position != b.Position {
		return a.Position < b.Position
	}
	return a.CreatedAt.Before(b.CreatedAt)
}

// partOf returns an item's archive set (lowercased GroupBase) and its
// part number, if it is a numbered archive part.
func partOf(it *Item) (set string, n int, ok bool) {
	if it.Filename == "" || !extract.IsArchive(it.Filename) {
		return "", 0, false
	}
	n, ok = extract.PartNumber(it.Filename)
	if !ok {
		return "", 0, false
	}
	return strings.ToLower(extract.GroupBase(it.Filename)), n, true
}
