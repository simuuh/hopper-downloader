package queue

import (
	"testing"
	"time"
)

// A subscriber that falls behind is closed instead of silently losing
// events, so the Web UI reconnects and reloads (M21).
func TestPublish_ClosesSubscriberThatFellBehind(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{ParallelFiles: 1})
	events, unsubscribe := mgr.Subscribe()
	defer unsubscribe()

	for i := 0; i < cap(events)+1; i++ {
		mgr.publish(Event{Type: EventItemUpdated, Item: &Item{ID: "x"}})
	}
	n := 0
	for range events { // ends only if the channel was closed
		n++
	}
	if n != cap(events) {
		t.Fatalf("received %d events before the close, want the %d that fit", n, cap(events))
	}
}

// An update of an item that has been removed in the meantime is not
// published: it would bring the item back in the Web UI (M21).
func TestPublishItem_SkipsRemovedItem(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{ParallelFiles: 1})
	it := &Item{ID: newID(), Filename: "file.bin"}
	injectDoneItem(t, mgr, it)
	stale, _ := mgr.Get(it.ID)
	if err := mgr.Cancel(it.ID); err != nil {
		t.Fatal(err)
	}

	events, unsubscribe := mgr.Subscribe()
	defer unsubscribe()
	mgr.publishItem(stale)
	select {
	case ev := <-events:
		t.Fatalf("published %+v for a removed item", ev)
	case <-time.After(50 * time.Millisecond):
	}
}
