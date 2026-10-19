package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"go.etcd.io/bbolt"
)

func newTestDB(t *testing.T) *bbolt.DB {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(t.TempDir(), "settings.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// defaultInitial mirrors what New() normalizes an initial value with a
// nil ExtractPasswords into (see New's doc comment) — a config-derived
// Settings never sets ExtractPasswords explicitly, so this is the
// realistic shape callers actually pass in.
func defaultInitial() Settings {
	return Settings{
		ParallelFiles:    3,
		ParallelChunks:   4,
		BandwidthLimit:   0,
		RetryMax:         5,
		AutoStartImports: false,
		DefaultPriority:  "normal",
		LogLevel:         "info",
		ExtractPasswords: []string{},
		AccountPriority:  []string{},
	}
}

func TestNew_PersistsInitialOnFirstRun(t *testing.T) {
	db := newTestDB(t)
	m, err := New(db, defaultInitial(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Get(); !reflect.DeepEqual(got, defaultInitial()) {
		t.Errorf("Get() = %+v, want %+v", got, defaultInitial())
	}
}

func TestNew_LoadsPersistedOverInitialOnRestart(t *testing.T) {
	db := newTestDB(t)
	m1, err := New(db, defaultInitial(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	changed := 7
	if _, err := m1.Apply(Update{ParallelFiles: &changed}); err != nil {
		t.Fatal(err)
	}

	// Simulate a restart: New again with the *original* initial (as if
	// hopper.yaml never changed) — the persisted override must win.
	m2, err := New(db, defaultInitial(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if got := m2.Get().ParallelFiles; got != 7 {
		t.Errorf("ParallelFiles after restart = %d, want 7 (persisted override)", got)
	}
}

func TestNew_InvokesHooksOnceAtStartup(t *testing.T) {
	db := newTestDB(t)
	var gotParallelFiles int
	_, err := New(db, defaultInitial(), Hooks{
		SetParallelFiles: func(n int) { gotParallelFiles = n },
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotParallelFiles != 3 {
		t.Errorf("hook received %d, want 3 (from initial)", gotParallelFiles)
	}
}

func TestApply_PartialUpdateLeavesOtherFieldsUnchanged(t *testing.T) {
	db := newTestDB(t)
	m, err := New(db, defaultInitial(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	n := 8
	updated, err := m.Apply(Update{ParallelFiles: &n})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ParallelFiles != 8 {
		t.Errorf("ParallelFiles = %d, want 8", updated.ParallelFiles)
	}
	if updated.ParallelChunks != 4 || updated.RetryMax != 5 || updated.LogLevel != "info" {
		t.Errorf("other fields changed unexpectedly: %+v", updated)
	}
}

func TestApply_RejectsOutOfRangeValues(t *testing.T) {
	db := newTestDB(t)
	m, err := New(db, defaultInitial(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}

	cases := []Update{
		{ParallelFiles: intPtr(0)},
		{ParallelFiles: intPtr(11)},
		{ParallelChunks: intPtr(0)},
		{ParallelChunks: intPtr(17)},
		{BandwidthLimit: int64Ptr(-1)},
		{RetryMax: intPtr(-1)},
		{RetryMax: intPtr(11)},
		{DefaultPriority: strPtr("urgent")},
		{LogLevel: strPtr("verbose")},
	}
	for _, u := range cases {
		if _, err := m.Apply(u); err == nil {
			t.Errorf("Apply(%+v) succeeded, want a validation error", u)
		}
	}
	// Rejected updates must not have partially applied.
	if got := m.Get(); !reflect.DeepEqual(got, defaultInitial()) {
		t.Errorf("settings changed after rejected updates: %+v", got)
	}
}

func TestApply_InvalidFieldRejectsWholeUpdate(t *testing.T) {
	db := newTestDB(t)
	m, err := New(db, defaultInitial(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	n := 9
	bad := 99
	if _, err := m.Apply(Update{ParallelFiles: &n, RetryMax: &bad}); err == nil {
		t.Fatal("expected an error from the invalid retry_max")
	}
	if got := m.Get().ParallelFiles; got != 3 {
		t.Errorf("ParallelFiles = %d, want unchanged at 3 (whole update should have been rejected)", got)
	}
}

func TestApply_CallsHooksForNewValue(t *testing.T) {
	db := newTestDB(t)
	var gotLevel string
	m, err := New(db, defaultInitial(), Hooks{
		SetLogLevel: func(level string) error {
			gotLevel = level
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	debug := "debug"
	if _, err := m.Apply(Update{LogLevel: &debug}); err != nil {
		t.Fatal(err)
	}
	if gotLevel != "debug" {
		t.Errorf("SetLogLevel hook received %q, want debug", gotLevel)
	}
}

func TestApply_DownloadPath_ValidDirectoryAppliesAndCallsHook(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	var gotPath string
	m, err := New(db, defaultInitial(), Hooks{
		SetDownloadPath: func(p string) { gotPath = p },
	})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := m.Apply(Update{DownloadPath: &dir})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if updated.DownloadPath != dir {
		t.Errorf("DownloadPath = %q, want %q", updated.DownloadPath, dir)
	}
	if gotPath != dir {
		t.Errorf("SetDownloadPath hook received %q, want %q (should apply live, no restart)", gotPath, dir)
	}
}

func TestApply_DownloadPath_NonexistentRejectedWithClearError(t *testing.T) {
	db := newTestDB(t)
	m, err := New(db, defaultInitial(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	_, err = m.Apply(Update{DownloadPath: &missing})
	if err == nil {
		t.Fatal("expected an error for a nonexistent download_path")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %q, want it to mention the path doesn't exist", err.Error())
	}
	if got := m.Get().DownloadPath; got != "" {
		t.Errorf("DownloadPath = %q, want unchanged (rejected update)", got)
	}
}

func TestApply_DownloadPath_RelativeRejected(t *testing.T) {
	db := newTestDB(t)
	m, err := New(db, defaultInitial(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	rel := "relative/path"
	if _, err := m.Apply(Update{DownloadPath: &rel}); err == nil {
		t.Fatal("expected an error for a relative download_path")
	}
}

func TestApply_DownloadPath_FileNotDirectoryRejected(t *testing.T) {
	db := newTestDB(t)
	m, err := New(db, defaultInitial(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(Update{DownloadPath: &file}); err == nil {
		t.Fatal("expected an error for a download_path that is a file, not a directory")
	}
}

func TestApply_DownloadPath_NotWritableRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory permission bits don't reliably block writes the way Unix mode bits do")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory mode bits, so a read-only directory stays writable")
	}
	db := newTestDB(t)
	m, err := New(db, defaultInitial(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// G302 doesn't apply here: this chmod deliberately makes dir
	// read-only (no write bit) to exercise Apply's writability probe —
	// 0600 would remove the execute bit a directory needs to remain
	// traversable at all, defeating the test's own purpose.
	if err := os.Chmod(dir, 0o555); err != nil { //nolint:gosec // G302: read-only directory permissions are the point of this test, not a leftover default
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) //nolint:gosec // G302: restoring perms so t.TempDir() can clean up, not a real file write

	if _, err := m.Apply(Update{DownloadPath: &dir}); err == nil {
		t.Fatal("expected an error for a read-only download_path")
	}
}

// TestNew_NormalizesNilExtractPasswordsToEmptySlice guards against a real
// bug caught via manual Web UI testing: config.ExtractConfig.Passwords is
// nil by default (hopper.yaml has no reason to ship one), and a nil Go
// slice marshals to JSON `null`, not `[]`. The Web UI reads
// settings.extract_passwords.length unconditionally on the Settings page,
// so a null response there is a hard crash, not just an empty list.
func TestNew_NormalizesNilExtractPasswordsToEmptySlice(t *testing.T) {
	db := newTestDB(t)
	initial := defaultInitial()
	initial.ExtractPasswords = nil
	m, err := New(db, initial, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Get().ExtractPasswords; got == nil {
		t.Error("Get().ExtractPasswords is nil, want a non-nil empty slice (marshals to JSON null, crashing the Web UI)")
	}
	data, err := json.Marshal(m.Get())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"extract_passwords":null`) {
		t.Errorf("marshaled settings contain extract_passwords:null: %s", data)
	}
}

func intPtr(n int) *int       { return &n }
func int64Ptr(n int64) *int64 { return &n }
func strPtr(s string) *string { return &s }

func TestApply_AccountPriority(t *testing.T) {
	m, err := New(newTestDB(t), defaultInitial(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}

	got, err := m.Apply(Update{AccountPriority: []string{" alldebrid ", "realdebrid"}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !reflect.DeepEqual(got.AccountPriority, []string{"alldebrid", "realdebrid"}) {
		t.Errorf("AccountPriority = %v", got.AccountPriority)
	}

	for _, bad := range [][]string{{"alldebrid", "alldebrid"}, {""}, {"Real Debrid"}} {
		if _, err := m.Apply(Update{AccountPriority: bad}); err == nil {
			t.Errorf("Apply(%q) accepted", bad)
		}
	}
	if got := m.Get().AccountPriority; !reflect.DeepEqual(got, []string{"alldebrid", "realdebrid"}) {
		t.Errorf("rejected update changed the order: %v", got)
	}

	// An explicit empty list restores the default order.
	got, err = m.Apply(Update{AccountPriority: []string{}})
	if err != nil || len(got.AccountPriority) != 0 {
		t.Errorf("clearing: %v, %v", got.AccountPriority, err)
	}
}

func TestUpdateCheck_OffByDefaultAndPersisted(t *testing.T) {
	db := newTestDB(t)
	m, err := New(db, defaultInitial(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Get().UpdateCheck {
		t.Fatal("UpdateCheck must be off unless the user turns it on")
	}
	on := true
	if _, err := m.Apply(Update{UpdateCheck: &on}); err != nil {
		t.Fatal(err)
	}
	m2, err := New(db, defaultInitial(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if !m2.Get().UpdateCheck {
		t.Error("UpdateCheck not persisted across restart")
	}
}
