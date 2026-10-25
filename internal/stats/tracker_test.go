package stats

import (
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func newTestDB(t *testing.T) *bbolt.DB {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(t.TempDir(), "stats.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRecordDownload_UpdatesSessionTodayAndAllTime(t *testing.T) {
	tr := NewTracker(newTestDB(t), nil)

	tr.RecordDownload("rapidgator", 1000, 500)
	tr.RecordDownload("realdebrid", 2000, 1500)

	sess := tr.Session()
	if sess.BytesTotal != 3000 {
		t.Errorf("session.BytesTotal = %d, want 3000", sess.BytesTotal)
	}
	if sess.FilesTotal != 2 {
		t.Errorf("session.FilesTotal = %d, want 2", sess.FilesTotal)
	}
	if sess.PeakSpeed != 1500 {
		t.Errorf("session.PeakSpeed = %d, want 1500", sess.PeakSpeed)
	}
	if sess.ByHoster["rapidgator"] != 1000 || sess.ByHoster["realdebrid"] != 2000 {
		t.Errorf("session.ByHoster = %+v, want rapidgator=1000 realdebrid=2000", sess.ByHoster)
	}

	today := tr.Today()
	if today.BytesTotal != 3000 || today.FilesTotal != 2 {
		t.Errorf("today = %+v, want bytes=3000 files=2", today)
	}
	if today.Date != time.Now().Format(dateLayout) {
		t.Errorf("today.Date = %q, want today's date", today.Date)
	}

	all := tr.AllTime()
	if all.BytesTotal != 3000 || all.FilesTotal != 2 {
		t.Errorf("alltime = %+v, want bytes=3000 files=2", all)
	}
	if all.PeakSpeed != 1500 {
		t.Errorf("alltime.PeakSpeed = %d, want 1500", all.PeakSpeed)
	}
	if all.LargestFile != 2000 {
		t.Errorf("alltime.LargestFile = %d, want 2000", all.LargestFile)
	}
}

func TestRecordError_UpdatesSessionTodayAndAllTime(t *testing.T) {
	tr := NewTracker(newTestDB(t), nil)

	tr.RecordError("rapidgator")
	tr.RecordError("rapidgator")

	if sess := tr.Session(); sess.Errors != 2 {
		t.Errorf("session.Errors = %d, want 2", sess.Errors)
	}
	if today := tr.Today(); today.Errors != 2 {
		t.Errorf("today.Errors = %d, want 2", today.Errors)
	}
	if all := tr.AllTime(); all.Errors != 2 {
		t.Errorf("alltime.Errors = %d, want 2", all.Errors)
	}
}

func TestSession_PersistsAcrossDownloadsWithinProcess(t *testing.T) {
	tr := NewTracker(newTestDB(t), nil)
	before := tr.Session().StartedAt
	if before.IsZero() {
		t.Fatal("StartedAt should be set immediately by NewTracker")
	}
	tr.RecordDownload("directhttp", 42, 10)
	after := tr.Session().StartedAt
	if !before.Equal(after) {
		t.Errorf("StartedAt changed across calls: %v -> %v", before, after)
	}
}

func TestSession_RestartLosesData(t *testing.T) {
	db := newTestDB(t)
	tr1 := NewTracker(db, nil)
	tr1.RecordDownload("directhttp", 500, 100)

	// A fresh Tracker over the same db simulates a process restart —
	// session stats are in-memory only and must not survive it, while
	// daily/all-time totals (bbolt-backed) must.
	tr2 := NewTracker(db, nil)
	if sess := tr2.Session(); sess.BytesTotal != 0 || sess.FilesTotal != 0 {
		t.Errorf("new Tracker's session = %+v, want zero", sess)
	}
	if today := tr2.Today(); today.BytesTotal != 500 {
		t.Errorf("today.BytesTotal after restart = %d, want 500 (persisted)", today.BytesTotal)
	}
	if all := tr2.AllTime(); all.BytesTotal != 500 {
		t.Errorf("alltime.BytesTotal after restart = %d, want 500 (persisted)", all.BytesTotal)
	}
}

func TestPeriod_AggregatesAndFillsMissingDays(t *testing.T) {
	tr := NewTracker(newTestDB(t), nil)
	tr.RecordDownload("rapidgator", 100, 10)
	tr.RecordError("rapidgator")

	agg := tr.Period(7)
	if agg.Days != 7 {
		t.Errorf("Days = %d, want 7", agg.Days)
	}
	if len(agg.Daily) != 7 {
		t.Fatalf("len(Daily) = %d, want 7", len(agg.Daily))
	}
	if agg.BytesTotal != 100 {
		t.Errorf("BytesTotal = %d, want 100", agg.BytesTotal)
	}
	if agg.Errors != 1 {
		t.Errorf("Errors = %d, want 1", agg.Errors)
	}
	if agg.ByHoster["rapidgator"] != 100 {
		t.Errorf("ByHoster[rapidgator] = %d, want 100", agg.ByHoster["rapidgator"])
	}

	// Every day except today should be a zero-valued placeholder with
	// just its Date set — no gap-filling needed by the caller.
	last := agg.Daily[len(agg.Daily)-1]
	if last.Date != time.Now().Format(dateLayout) {
		t.Errorf("last daily entry date = %q, want today", last.Date)
	}
	if last.BytesTotal != 100 {
		t.Errorf("last daily entry BytesTotal = %d, want 100", last.BytesTotal)
	}
	for i, d := range agg.Daily[:len(agg.Daily)-1] {
		if d.BytesTotal != 0 || d.FilesTotal != 0 || d.Errors != 0 {
			t.Errorf("Daily[%d] = %+v, want zero-valued placeholder", i, d)
		}
		if d.Date == "" {
			t.Errorf("Daily[%d].Date is empty, want a date", i)
		}
	}
}

func TestAllTime_TracksPeakSpeedAndLargestFileAcrossCalls(t *testing.T) {
	tr := NewTracker(newTestDB(t), nil)
	tr.RecordDownload("a", 100, 500)
	tr.RecordDownload("b", 900, 200) // bigger file, slower speed
	tr.RecordDownload("c", 50, 900)  // fastest speed, smallest file

	all := tr.AllTime()
	if all.PeakSpeed != 900 {
		t.Errorf("PeakSpeed = %d, want 900", all.PeakSpeed)
	}
	if all.LargestFile != 900 {
		t.Errorf("LargestFile = %d, want 900", all.LargestFile)
	}
}

func TestToday_EmptyTrackerReturnsZeroValueWithDateSet(t *testing.T) {
	tr := NewTracker(newTestDB(t), nil)
	today := tr.Today()
	if today.BytesTotal != 0 || today.FilesTotal != 0 || today.Errors != 0 {
		t.Errorf("today = %+v, want all zero", today)
	}
	if today.Date != time.Now().Format(dateLayout) {
		t.Errorf("today.Date = %q, want today's date", today.Date)
	}
	if today.ByHoster == nil {
		t.Error("today.ByHoster should be a non-nil empty map, not nil")
	}
}

// A damaged all-time record is started over instead of blocking every
// later update (N19).
func TestRecordDownload_RecoversFromDamagedRecord(t *testing.T) {
	db := newTestDB(t)
	tr := NewTracker(db, nil)
	if err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketAllTime)
		if err != nil {
			return err
		}
		return b.Put(allTimeKey, []byte("{not json"))
	}); err != nil {
		t.Fatal(err)
	}

	tr.RecordDownload("example", 100, 10)
	if got := tr.AllTime(); got.FilesTotal != 1 || got.BytesTotal != 100 {
		t.Fatalf("all-time after a damaged record: files %d bytes %d, want 1 and 100", got.FilesTotal, got.BytesTotal)
	}
	if got := tr.Today(); got.FilesTotal != 1 {
		t.Fatalf("today's files = %d, want 1", got.FilesTotal)
	}
}
