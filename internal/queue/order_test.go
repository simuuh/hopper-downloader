package queue

import (
	"fmt"
	"testing"
)

// pickNext's ordering work grows linearly, not quadratically, with the
// number of parts (M26): 2000 parts used to take millions of regex
// matches per dispatch.
func BenchmarkPickNext_ManyParts(b *testing.B) {
	items := map[string]*Item{}
	for i := 0; i < 2000; i++ {
		id := fmt.Sprintf("id%04d", i)
		items[id] = &Item{ID: id, PackageID: "pkg", Filename: fmt.Sprintf("example.part%04d.rar", i+1), State: StateQueued, Position: int64(i)}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		order := newQueueOrder(items)
		blocked := order.sequentialBlocked(items)
		var best *Item
		for _, it := range items {
			if blocked(it) {
				continue
			}
			if best == nil || order.less(it, best) {
				best = it
			}
		}
		if best == nil || best.ID != "id0000" {
			b.Fatalf("picked %v, want part 1", best)
		}
	}
}
