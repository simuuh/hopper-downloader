package queue

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCancel_RemovesPartialDataOfUnfinishedItem(t *testing.T) {
	store := newTestStore(t)
	mgr, base := newTestManager(t, store, Config{})
	it, err := mgr.AddLink("http://example.invalid/big.iso", AddOptions{StartPaused: true, Filename: "big.iso"})
	if err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(base, "big.iso.part")
	state := filepath.Join(base, "big.iso.part.json")
	for _, p := range []string{part, state} {
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := mgr.Cancel(it.ID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{part, state} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after cancel", filepath.Base(p))
		}
	}
}

func TestCancel_KeepsCompletedFile(t *testing.T) {
	content := []byte("finished")
	srv := staticFileServer(t, content)
	store := newTestStore(t)
	mgr, base := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: 10 * time.Millisecond})
	mgr.Start(t.Context())
	it, _ := mgr.AddLink(srv.URL+"/done.txt", AddOptions{})
	waitForState(t, mgr, it.ID, StateDone, 3*time.Second)
	if err := mgr.Cancel(it.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "done.txt")); err != nil {
		t.Fatalf("completed file removed by cancel: %v", err)
	}
}

func TestCancel_RunningTransferPartialRemoved(t *testing.T) {
	srv := partialThenHangServer(t)
	store := newTestStore(t)
	mgr, base := newTestManager(t, store, Config{ParallelFiles: 1, PollInterval: 10 * time.Millisecond})
	mgr.Start(t.Context())
	it, _ := mgr.AddLink(srv.URL+"/slow.bin", AddOptions{Filename: "slow.bin"})
	waitForState(t, mgr, it.ID, StateDownloading, 3*time.Second)
	// Only meaningful once the transfer has actually created its data file.
	created := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(base, "slow.bin.part")); err == nil {
			break
		}
		if time.Now().After(created) {
			t.Fatal("transfer never created slow.bin.part")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := mgr.Cancel(it.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, e1 := os.Stat(filepath.Join(base, "slow.bin.part"))
		_, e2 := os.Stat(filepath.Join(base, "slow.bin.part.json"))
		if os.IsNotExist(e1) && os.IsNotExist(e2) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("partial data of a cancelled running transfer was not removed")
}

// partialThenHangServer answers HEAD immediately (1 MiB) and, on
// GET, sends 64 KiB and then keeps the connection open until the client
// gives up — a transfer that is definitely in progress with data on disk.
func partialThenHangServer(t *testing.T) *httptest.Server {
	t.Helper()
	const size = 1 << 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(make([]byte, 64<<10))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}
