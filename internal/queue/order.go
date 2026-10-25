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
	// parts holds every numbered archive part's set and number, keyed by
	// item ID: parsed once per order instead of on every comparison,
	// which with thousands of parts meant millions of regex matches per
	// dispatch, all under the queue lock.
	parts map[string]partInfo
}

type partInfo struct {
	set   string // lowercased GroupBase, for ordering
	group string // GroupBase as written, for sequential_parts
	n     int
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
	o := queueOrder{rank: make(map[string]int64), prio: make(map[string]Priority), parts: make(map[string]partInfo)}
	openPrio := make(map[string]Priority) // max priority of unfinished items
	allPrio := make(map[string]Priority)  // max priority of all items
	for _, it := range items {
		if p, ok := parsePart(it); ok {
			o.parts[it.ID] = p
		}
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
	if pa, ok := o.part(a); ok {
		if pb, ok := o.part(b); ok && pa.set == pb.set && pa.n != pb.n {
			return pa.n < pb.n
		}
	}
	if a.Position != b.Position {
		return a.Position < b.Position
	}
	return a.CreatedAt.Before(b.CreatedAt)
}

// part returns it's archive part info, from the precomputed table when
// it was in the item set the order was built from.
func (o queueOrder) part(it *Item) (partInfo, bool) {
	if p, ok := o.parts[it.ID]; ok {
		return p, true
	}
	if _, known := o.rank[groupKey(it)]; known && o.parts != nil {
		return partInfo{}, false // parsed already: not a numbered part
	}
	return parsePart(it)
}

// parsePart returns an item's archive set and part number, if it is a
// numbered archive part.
func parsePart(it *Item) (partInfo, bool) {
	if it.Filename == "" || !extract.IsArchive(it.Filename) {
		return partInfo{}, false
	}
	n, ok := extract.PartNumber(it.Filename)
	if !ok {
		return partInfo{}, false
	}
	group := extract.GroupBase(it.Filename)
	return partInfo{set: strings.ToLower(group), group: group, n: n}, true
}

// sequentialBlocked builds the sequential_parts check for one dispatch:
// an archive part is held back while a lower-numbered part of its set
// (same PackageID, same extract.GroupBase) is still queued, so parts
// never overtake each other. They may run side by side: once part1 has
// started, part2 can take the next free slot, so parallel_files fills the
// topmost package first instead of spreading over several packages.
//
// The order already picks parts by number; this covers a lower part that
// isn't runnable right now (e.g. waiting on a hoster's retry_after).
// StateError and StatePaused don't block, or one dead or paused part
// would strand the rest of the set. Built in one pass over the items,
// instead of a pass per candidate; call with Manager.mu held.
func (o queueOrder) sequentialBlocked(items map[string]*Item) func(*Item) bool {
	minQueued := map[string]int{} // package|group -> lowest queued part number
	for _, it := range items {
		p, ok := o.parts[it.ID]
		if !ok || it.PackageID == "" || it.State != StateQueued {
			continue
		}
		key := it.PackageID + "|" + p.group
		if n, seen := minQueued[key]; !seen || p.n < n {
			minQueued[key] = p.n
		}
	}
	return func(it *Item) bool {
		p, ok := o.parts[it.ID]
		if !ok || it.PackageID == "" {
			return false
		}
		n, seen := minQueued[it.PackageID+"|"+p.group]
		return seen && n < p.n
	}
}
