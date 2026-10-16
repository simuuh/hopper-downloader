package account

import (
	"testing"
	"time"

	"github.com/simuuh/hopper-downloader/plugins"
)

func int64p(v int64) *int64 { return &v }

func mustAdd(t *testing.T, m *Manager, hoster, user string) string {
	t.Helper()
	a, err := m.Add(hoster, user, "pw-"+user, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func TestAccountFor_SkipsInvalidExpiredAndExhausted(t *testing.T) {
	m := newTestManager(t, testKey)
	invalid := mustAdd(t, m, "rapidgator", "invalid")
	expired := mustAdd(t, m, "rapidgator", "expired")
	empty := mustAdd(t, m, "rapidgator", "empty")
	good := mustAdd(t, m, "rapidgator", "good")

	past := time.Now().Add(-time.Hour)
	mustValidate(t, m, invalid, &plugins.AccountInfo{Valid: false})
	mustValidate(t, m, expired, &plugins.AccountInfo{Valid: true, ExpiresAt: &past})
	mustValidate(t, m, empty, &plugins.AccountInfo{Valid: true, TrafficLeft: int64p(0)})
	mustValidate(t, m, good, &plugins.AccountInfo{Valid: true, TrafficLeft: int64p(1 << 30)})

	acc, ok := m.AccountFor("rapidgator")
	if !ok || acc.Username != "good" || acc.ID != good {
		t.Fatalf("AccountFor = %+v, %v; want the only usable account", acc, ok)
	}
}

func TestAccountFor_PrefersValidThenMostTraffic(t *testing.T) {
	m := newTestManager(t, testKey)
	unknown := mustAdd(t, m, "katfile", "unknown")
	little := mustAdd(t, m, "katfile", "little")
	lots := mustAdd(t, m, "katfile", "lots")
	_ = unknown
	mustValidate(t, m, little, &plugins.AccountInfo{Valid: true, TrafficLeft: int64p(100)})
	mustValidate(t, m, lots, &plugins.AccountInfo{Valid: true, TrafficLeft: int64p(5000)})

	acc, _ := m.AccountFor("katfile")
	if acc.Username != "lots" {
		t.Fatalf("AccountFor picked %q, want lots (valid, most traffic)", acc.Username)
	}
}

func TestReportFailure_RotatesAndRecovers(t *testing.T) {
	m := newTestManager(t, testKey)
	first := mustAdd(t, m, "realdebrid", "first")
	time.Sleep(2 * time.Millisecond) // distinct CreatedAt -> deterministic order
	second := mustAdd(t, m, "realdebrid", "second")

	acc, _ := m.AccountFor("realdebrid")
	if acc.ID != first {
		t.Fatalf("initial pick = %s, want oldest account", acc.Username)
	}

	if !m.ReportFailure(first) {
		t.Fatal("ReportFailure(first) = false")
	}
	acc, _ = m.AccountFor("realdebrid")
	if acc.ID != second {
		t.Fatalf("after failure pick = %s, want second", acc.Username)
	}
	if pub, _ := m.Get(first); pub.CooldownUntil == nil {
		t.Error("Get(first).CooldownUntil = nil, want set while cooling down")
	}

	// Both rejected: still returns an account (so the hoster stays
	// "configured"), not none.
	m.ReportFailure(second)
	if _, ok := m.AccountFor("realdebrid"); !ok {
		t.Fatal("AccountFor returned none with every account cooling down")
	}

	// A successful re-validation ends the cooldown early.
	mustValidate(t, m, first, &plugins.AccountInfo{Valid: true})
	if pub, _ := m.Get(first); pub.CooldownUntil != nil {
		t.Error("cooldown not cleared by a valid re-check")
	}
	acc, _ = m.AccountFor("realdebrid")
	if acc.ID != first {
		t.Fatalf("after re-check pick = %s, want first again", acc.Username)
	}
}

func mustValidate(t *testing.T, m *Manager, id string, info *plugins.AccountInfo) {
	t.Helper()
	if _, err := m.SetValidation(id, info); err != nil {
		t.Fatal(err)
	}
}
