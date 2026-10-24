package queue

import (
	"testing"
	"time"
)

// When the database can't delete an item, Cancel fails and the item
// stays, instead of vanishing until the next start brings it back (N18).
func TestCancel_DatabaseFailureKeepsItem(t *testing.T) {
	store := newTestStore(t)
	mgr, _ := newTestManager(t, store, Config{ParallelFiles: 1})
	it := &Item{ID: newID(), Filename: "file.bin"}
	injectDoneItem(t, mgr, it)

	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Cancel(it.ID); err == nil {
		t.Fatal("Cancel succeeded with the database closed")
	}
	if _, ok := mgr.Get(it.ID); !ok {
		t.Fatal("item removed from the queue although the database still has it")
	}
}

// Reorder publishes every moved item, so other open Web UIs follow (N18).
func TestReorder_PublishesMovedItems(t *testing.T) {
	mgr, _ := newTestManager(t, newTestStore(t), Config{ParallelFiles: 1})
	a := &Item{ID: newID(), Filename: "a.bin"}
	b := &Item{ID: newID(), Filename: "b.bin"}
	injectDoneItem(t, mgr, a)
	injectDoneItem(t, mgr, b)

	events, unsubscribe := mgr.Subscribe()
	defer unsubscribe()
	if err := mgr.Reorder([]string{b.ID, a.ID}); err != nil {
		t.Fatal(err)
	}

	got := map[string]int64{}
	timeout := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-events:
			if ev.Type == EventItemUpdated && ev.Item != nil {
				got[ev.Item.ID] = ev.Item.Position
			}
		case <-timeout:
			t.Fatalf("got updates for %d of 2 items", len(got))
		}
	}
	if got[b.ID] != 0 || got[a.ID] != 1 {
		t.Fatalf("published positions %v, want b=0 a=1", got)
	}
}
