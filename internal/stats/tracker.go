package stats

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

var (
	bucketDaily   = []byte("stats_daily")
	bucketAllTime = []byte("stats_alltime")
	allTimeKey    = []byte("v1")
)

// Tracker records completed/failed downloads and answers the queries the
// Web UI's stats dashboard needs. Buckets are created lazily on first
// write (CreateBucketIfNotExists), not in NewTracker, so construction
// itself never touches db.
//
// Daily and all-time totals are read-modify-write per event under a
// single bbolt.Update transaction (bbolt itself serializes writers), so
// concurrent RecordDownload/RecordError calls from multiple download
// workers never lose an update to a race. Session totals are additionally
// guarded by mu since they live in plain Go memory, not bbolt.
type Tracker struct {
	db  *bbolt.DB
	log *slog.Logger

	mu      sync.Mutex
	session SessionStats
}

// NewTracker builds a Tracker around db. log may be nil (tests); a nil
// log silently drops the rare persistence-error message a full disk or
// similar would produce, matching every Record* method's fire-and-forget
// contract — a failed persist never blocks or fails the download it was
// recording.
func NewTracker(db *bbolt.DB, log *slog.Logger) *Tracker {
	return &Tracker{
		db:  db,
		log: log,
		session: SessionStats{
			StartedAt: time.Now(),
			ByHoster:  map[string]int64{},
		},
	}
}

// RecordDownload records one completed download: bytes transferred,
// attributed to hosterKey, and the peak instantaneous speed observed
// during the transfer (downloader.Result.PeakSpeed).
func (t *Tracker) RecordDownload(hosterKey string, bytes int64, speed int64) {
	t.mu.Lock()
	t.session.BytesTotal += bytes
	t.session.FilesTotal++
	if speed > t.session.PeakSpeed {
		t.session.PeakSpeed = speed
	}
	t.session.ByHoster[hosterKey] += bytes
	t.mu.Unlock()

	date := time.Now().Format(dateLayout)
	if err := t.updateDaily(date, func(d *DailyStats) {
		d.BytesTotal += bytes
		d.FilesTotal++
		d.ByHoster[hosterKey] += bytes
	}); err != nil {
		t.logError("persist daily stats", err)
	}

	if err := t.updateAllTime(func(a *AllTimeStats) {
		a.BytesTotal += bytes
		a.FilesTotal++
		if speed > a.PeakSpeed {
			a.PeakSpeed = speed
		}
		if bytes > a.LargestFile {
			a.LargestFile = bytes
		}
		a.ByHoster[hosterKey] += bytes
	}); err != nil {
		t.logError("persist all-time stats", err)
	}
}

// RecordError records one permanently-failed download, attributed to
// hosterKey (which may be empty — a failure before any plugin matched
// the URL never had a hoster to attribute to).
func (t *Tracker) RecordError(hosterKey string) {
	t.mu.Lock()
	t.session.Errors++
	t.mu.Unlock()

	date := time.Now().Format(dateLayout)
	if err := t.updateDaily(date, func(d *DailyStats) {
		d.Errors++
	}); err != nil {
		t.logError("persist daily error stats", err)
	}

	if err := t.updateAllTime(func(a *AllTimeStats) {
		a.Errors++
	}); err != nil {
		t.logError("persist all-time error stats", err)
	}
}

func (t *Tracker) logError(action string, err error) {
	if t.log != nil {
		t.log.Error("stats: "+action, "error", err)
	}
}

// Today returns today's DailyStats (server-local date), or a zero-valued
// one (with just Date set) if nothing has been recorded yet today.
func (t *Tracker) Today() DailyStats {
	return t.dayStats(time.Now().Format(dateLayout))
}

// Period aggregates the last days calendar days, ending today
// (inclusive). Daily contains one entry per day in the range, oldest
// first, so callers can chart the range without filling gaps themselves.
func (t *Tracker) Period(days int) AggregateStats {
	if days < 1 {
		days = 1
	}
	now := time.Now()
	dates := make([]string, days)
	for i := 0; i < days; i++ {
		dates[i] = now.AddDate(0, 0, -(days - 1 - i)).Format(dateLayout)
	}

	agg := AggregateStats{Days: days, ByHoster: map[string]int64{}}
	daily := make([]DailyStats, len(dates))

	_ = t.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketDaily)
		for i, date := range dates {
			d := DailyStats{Date: date, ByHoster: map[string]int64{}}
			if b != nil {
				if v := b.Get([]byte(date)); v != nil {
					_ = json.Unmarshal(v, &d)
					if d.ByHoster == nil {
						d.ByHoster = map[string]int64{}
					}
				}
			}
			daily[i] = d
		}
		return nil
	})

	for _, d := range daily {
		agg.BytesTotal += d.BytesTotal
		agg.FilesTotal += d.FilesTotal
		agg.Errors += d.Errors
		for h, b := range d.ByHoster {
			agg.ByHoster[h] += b
		}
	}
	agg.Daily = daily
	return agg
}

// AllTime returns the running total since Hopper was first set up.
func (t *Tracker) AllTime() AllTimeStats {
	a := AllTimeStats{ByHoster: map[string]int64{}}
	_ = t.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketAllTime)
		if b == nil {
			return nil
		}
		v := b.Get(allTimeKey)
		if v == nil {
			return nil
		}
		return json.Unmarshal(v, &a)
	})
	if a.ByHoster == nil {
		a.ByHoster = map[string]int64{}
	}
	return a
}

// Session returns activity since this process started. StartedAt is
// never zero: NewTracker sets it immediately.
func (t *Tracker) Session() SessionStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	cp := t.session
	cp.ByHoster = make(map[string]int64, len(t.session.ByHoster))
	for k, v := range t.session.ByHoster {
		cp.ByHoster[k] = v
	}
	return cp
}

func (t *Tracker) dayStats(date string) DailyStats {
	d := DailyStats{Date: date, ByHoster: map[string]int64{}}
	_ = t.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketDaily)
		if b == nil {
			return nil
		}
		v := b.Get([]byte(date))
		if v == nil {
			return nil
		}
		return json.Unmarshal(v, &d)
	})
	if d.ByHoster == nil {
		d.ByHoster = map[string]int64{}
	}
	return d
}

func (t *Tracker) updateDaily(date string, mutate func(*DailyStats)) error {
	return t.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketDaily)
		if err != nil {
			return fmt.Errorf("stats: create daily bucket: %w", err)
		}
		d := DailyStats{Date: date, ByHoster: map[string]int64{}}
		if v := b.Get([]byte(date)); v != nil {
			if err := json.Unmarshal(v, &d); err != nil {
				return fmt.Errorf("stats: unmarshal daily stats: %w", err)
			}
			if d.ByHoster == nil {
				d.ByHoster = map[string]int64{}
			}
		}
		mutate(&d)
		data, err := json.Marshal(d)
		if err != nil {
			return fmt.Errorf("stats: marshal daily stats: %w", err)
		}
		return b.Put([]byte(date), data)
	})
}

func (t *Tracker) updateAllTime(mutate func(*AllTimeStats)) error {
	return t.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketAllTime)
		if err != nil {
			return fmt.Errorf("stats: create all-time bucket: %w", err)
		}
		a := AllTimeStats{ByHoster: map[string]int64{}}
		if v := b.Get(allTimeKey); v != nil {
			if err := json.Unmarshal(v, &a); err != nil {
				return fmt.Errorf("stats: unmarshal all-time stats: %w", err)
			}
			if a.ByHoster == nil {
				a.ByHoster = map[string]int64{}
			}
		}
		mutate(&a)
		data, err := json.Marshal(a)
		if err != nil {
			return fmt.Errorf("stats: marshal all-time stats: %w", err)
		}
		return b.Put(allTimeKey, data)
	})
}
