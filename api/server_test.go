package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"

	"github.com/simuuh/hopper-downloader/internal/account"
	"github.com/simuuh/hopper-downloader/internal/dlc"
	"github.com/simuuh/hopper-downloader/internal/queue"
	"github.com/simuuh/hopper-downloader/internal/settings"
	"github.com/simuuh/hopper-downloader/internal/stats"
	"github.com/simuuh/hopper-downloader/plugins"
)

// ---- fakes ----

// fakeCredentialedHoster is a minimal plugins.Hoster that also implements
// plugins.Credentialed (returning true), standing in for a real
// account-requiring plugin like rapidgator/realdebrid in tests.
type fakeCredentialedHoster struct{ name string }

func (f *fakeCredentialedHoster) Name() string      { return f.name }
func (f *fakeCredentialedHoster) Domains() []string { return []string{f.name + ".example.com"} }
func (f *fakeCredentialedHoster) Match(string) bool { return false }
func (f *fakeCredentialedHoster) Resolve(url string, acc *plugins.Account) (*plugins.FileInfo, error) {
	return nil, nil
}
func (f *fakeCredentialedHoster) FetchDownloadURL(url string, acc *plugins.Account) (string, error) {
	return "", nil
}
func (f *fakeCredentialedHoster) Credentialed() bool { return true }

// fakeUncredentialedHoster implements plugins.Hoster only, like directhttp:
// no Credentialed() method at all, so it must never appear in
// GET /api/v1/plugins or be accepted as an addAccountRequest.HosterKey.
type fakeUncredentialedHoster struct{ name string }

func (f *fakeUncredentialedHoster) Name() string      { return f.name }
func (f *fakeUncredentialedHoster) Domains() []string { return nil }
func (f *fakeUncredentialedHoster) Match(string) bool { return true }
func (f *fakeUncredentialedHoster) Resolve(url string, acc *plugins.Account) (*plugins.FileInfo, error) {
	return nil, nil
}
func (f *fakeUncredentialedHoster) FetchDownloadURL(url string, acc *plugins.Account) (string, error) {
	return "", nil
}

// fakeValidatingHoster additionally implements plugins.AccountValidator,
// standing in for realdebrid in tests that exercise inline/on-demand
// account validation. validateFn lets each test control the outcome
// (success, hoster-reported-invalid, or a hoster-call error) without a
// real HTTP server; nil defaults to a successful, valid result.
type fakeValidatingHoster struct {
	fakeCredentialedHoster
	validateFn func(acc *plugins.Account) (*plugins.AccountInfo, error)
}

func (f *fakeValidatingHoster) ValidateAccount(acc *plugins.Account) (*plugins.AccountInfo, error) {
	if f.validateFn != nil {
		return f.validateFn(acc)
	}
	return &plugins.AccountInfo{Valid: true, Label: "Premium"}, nil
}

type fakeQueue struct {
	mu       sync.Mutex
	items    map[string]*queue.Item
	packages []*queue.Package
	seq      int

	addLinkErr, addPackageErr                            error
	pauseErr, resumeErr, retryErr, cancelErr, reorderErr error
	retryExtractErr                                      error
	lastRetryExtractPassword                             string
	solveCaptchaErr                                      error
	lastCaptchaSolution                                  string
	lastAddLinksName                                     string
	lastPackageAction                                    string

	subs []chan queue.Event
}

func newFakeQueue() *fakeQueue { return &fakeQueue{items: map[string]*queue.Item{}} }

func (f *fakeQueue) nextID() string {
	f.seq++
	return fmt.Sprintf("item-%d", f.seq)
}

func (f *fakeQueue) AddLink(rawURL string, opts queue.AddOptions) (*queue.Item, error) {
	if f.addLinkErr != nil {
		return nil, f.addLinkErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	state := queue.StateQueued
	if opts.StartPaused {
		state = queue.StatePaused
	}
	it := &queue.Item{ID: f.nextID(), URL: rawURL, Priority: opts.Priority, State: state, Password: opts.Password}
	f.items[it.ID] = it
	return it, nil
}

func (f *fakeQueue) AddPackage(name string, urls []string, opts queue.AddOptions) (*queue.Package, []*queue.Item, error) {
	if f.addPackageErr != nil {
		return nil, nil, f.addPackageErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	pkg := &queue.Package{ID: f.nextID(), Name: name, Password: opts.Password}
	f.packages = append(f.packages, pkg)
	state := queue.StateQueued
	if opts.StartPaused {
		state = queue.StatePaused
	}
	items := make([]*queue.Item, 0, len(urls))
	for i, u := range urls {
		filename := ""
		if i < len(opts.Filenames) {
			filename = opts.Filenames[i]
		}
		it := &queue.Item{ID: f.nextID(), URL: u, PackageID: pkg.ID, State: state, Filename: filename}
		f.items[it.ID] = it
		items = append(items, it)
	}
	return pkg, items, nil
}

func (f *fakeQueue) AddLinks(links []queue.LinkInput, name string, opts queue.AddOptions) (*queue.AddResult, error) {
	urls := make([]string, 0, len(links))
	names := make([]string, 0, len(links))
	for _, l := range links {
		if !strings.HasPrefix(l.URL, "http://") && !strings.HasPrefix(l.URL, "https://") {
			continue
		}
		urls = append(urls, l.URL)
		names = append(names, l.Filename)
	}
	if len(urls) == 0 {
		return nil, queue.ErrNoLinks
	}
	f.mu.Lock()
	f.lastAddLinksName = name
	f.mu.Unlock()
	opts.Filenames = names
	pkg, items, err := f.AddPackage(name, urls, opts)
	if err != nil {
		return nil, err
	}
	return &queue.AddResult{Packages: []*queue.Package{pkg}, Items: items, Duplicates: []string{}}, nil
}

func (f *fakeQueue) RenamePackage(id, name string) (*queue.Package, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.packages {
		if p.ID == id {
			p.Name = name
			cp := *p
			return &cp, nil
		}
	}
	return nil, queue.ErrNotFound
}

func (f *fakeQueue) ApplyAction(ids []string, action queue.PackageAction) (int, error) {
	switch action {
	case queue.PackageActionPause, queue.PackageActionResume, queue.PackageActionRetry, queue.PackageActionDelete:
	default:
		return 0, queue.ErrInvalidAction
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPackageAction = string(action)
	return len(ids), nil
}

func (f *fakeQueue) ApplyPackageAction(id string, action queue.PackageAction) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPackageAction = string(action)
	n := 0
	for itemID, it := range f.items {
		if it.PackageID != id {
			continue
		}
		n++
		if action == queue.PackageActionDelete {
			delete(f.items, itemID)
		}
	}
	if n == 0 {
		return 0, queue.ErrNotFound
	}
	return n, nil
}

func (f *fakeQueue) PauseAll() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range f.items {
		if it.State == queue.StateQueued || it.State == queue.StateDownloading {
			it.State = queue.StatePaused
		}
	}
	return nil
}

func (f *fakeQueue) ResumeAll() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range f.items {
		if it.State == queue.StatePaused {
			it.State = queue.StateQueued
		}
	}
	return nil
}

func (f *fakeQueue) SetPriority(id string, p queue.Priority) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[id]
	if !ok {
		return queue.ErrNotFound
	}
	it.Priority = p
	return nil
}

func (f *fakeQueue) EditItem(id string, e queue.ItemEdit) (*queue.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[id]
	if !ok {
		return nil, queue.ErrNotFound
	}
	if it.State == queue.StateDone || it.State == queue.StateDownloading {
		return nil, queue.ErrItemNotEditable
	}
	if e.URL != nil {
		it.URL = *e.URL
		if it.State == queue.StateError {
			it.State = queue.StateQueued
		}
	}
	if e.Filename != nil {
		it.Filename = *e.Filename
	}
	cp := *it
	return &cp, nil
}

func (f *fakeQueue) AddToPackage(id string, links []queue.LinkInput, opts queue.AddOptions) (*queue.AddResult, error) {
	f.mu.Lock()
	var pkg *queue.Package
	for _, p := range f.packages {
		if p.ID == id {
			pkg = p
		}
	}
	f.mu.Unlock()
	if pkg == nil {
		return nil, queue.ErrNotFound
	}
	res := &queue.AddResult{Packages: []*queue.Package{pkg}, Duplicates: []string{}}
	for _, l := range links {
		it, err := f.AddLink(l.URL, opts)
		if err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.items[it.ID].PackageID = id
		f.mu.Unlock()
		it.PackageID = id
		res.Items = append(res.Items, it)
	}
	return res, nil
}

func (f *fakeQueue) SetPackagePath(id, path string) (*queue.Package, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.packages {
		if p.ID == id {
			p.Path = path
			return p, nil
		}
	}
	return nil, queue.ErrNotFound
}

func (f *fakeQueue) List() []*queue.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*queue.Item, 0, len(f.items))
	for _, it := range f.items {
		out = append(out, it)
	}
	return out
}

func (f *fakeQueue) Get(id string) (*queue.Item, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[id]
	return it, ok
}

func (f *fakeQueue) ListPackages() []*queue.Package { return f.packages }

func (f *fakeQueue) Pause(id string) error  { return f.actionErr(f.pauseErr, id, queue.StatePaused) }
func (f *fakeQueue) Resume(id string) error { return f.actionErr(f.resumeErr, id, queue.StateQueued) }
func (f *fakeQueue) Retry(id string) error  { return f.actionErr(f.retryErr, id, queue.StateQueued) }

func (f *fakeQueue) actionErr(injected error, id string, newState queue.State) error {
	if injected != nil {
		return injected
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[id]
	if !ok {
		return queue.ErrNotFound
	}
	it.State = newState
	return nil
}

func (f *fakeQueue) Cancel(id string) error {
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[id]; !ok {
		return queue.ErrNotFound
	}
	delete(f.items, id)
	return nil
}

func (f *fakeQueue) CancelMany(ids []string) (deleted []string, failed []string) {
	deleted = make([]string, 0, len(ids))
	failed = make([]string, 0)
	for _, id := range ids {
		if err := f.Cancel(id); err != nil {
			failed = append(failed, id)
			continue
		}
		deleted = append(deleted, id)
	}
	return deleted, failed
}

func (f *fakeQueue) RetryExtraction(id, password string) error {
	if f.retryExtractErr != nil {
		return f.retryExtractErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[id]
	if !ok {
		return queue.ErrNotFound
	}
	if it.State != queue.StateDone || it.ExtractState != queue.ExtractStateExtractFailed {
		return queue.ErrExtractNotRetryable
	}
	f.lastRetryExtractPassword = password
	it.ExtractState = queue.ExtractStateExtracted
	it.ExtractError = ""
	return nil
}

func (f *fakeQueue) SolveCaptcha(id, solution string) error {
	if f.solveCaptchaErr != nil {
		return f.solveCaptchaErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[id]
	if !ok {
		return queue.ErrNotFound
	}
	if it.CaptchaURL == "" {
		return queue.ErrNoCaptchaPending
	}
	f.lastCaptchaSolution = solution
	it.CaptchaURL = ""
	return nil
}

func (f *fakeQueue) Reorder(ids []string) error { return f.reorderErr }

func (f *fakeQueue) Subscribe() (<-chan queue.Event, func()) {
	ch := make(chan queue.Event, 8)
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	return ch, func() {}
}

func (f *fakeQueue) publish(ev queue.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.subs {
		ch <- ev
	}
}

type fakeAccounts struct {
	mu       sync.Mutex
	accounts map[string]*account.Account
	creds    map[string]*plugins.Account
	seq      int
	addErr   error
}

func newFakeAccounts() *fakeAccounts {
	return &fakeAccounts{accounts: map[string]*account.Account{}, creds: map[string]*plugins.Account{}}
}

func (f *fakeAccounts) Add(hosterKey, username, password string, extra map[string]string) (*account.Account, error) {
	if f.addErr != nil {
		return nil, f.addErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	acc := &account.Account{ID: fmt.Sprintf("acc-%d", f.seq), HosterKey: hosterKey, Username: username, Enabled: true}
	f.accounts[acc.ID] = acc
	f.creds[acc.ID] = &plugins.Account{Username: username, Password: password, Extra: extra}
	return acc, nil
}

func (f *fakeAccounts) UpdateCredentials(id, username, password string, extra map[string]string) (*account.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	acc, ok := f.accounts[id]
	if !ok {
		return nil, account.ErrNotFound
	}
	c := f.creds[id]
	if username != "" {
		acc.Username, c.Username = username, username
	}
	if password != "" {
		c.Password = password
	}
	acc.Valid = nil
	cp := *acc
	return &cp, nil
}

func (f *fakeAccounts) Credentials(id string) (*plugins.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.creds[id]
	if !ok {
		return nil, account.ErrNotFound
	}
	return c, nil
}

func (f *fakeAccounts) SetValidation(id string, info *plugins.AccountInfo) (*account.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[id]
	if !ok {
		return nil, account.ErrNotFound
	}
	now := time.Now()
	a.ValidatedAt = &now
	if info != nil {
		valid := info.Valid
		a.Valid = &valid
		a.ExpiresAt = info.ExpiresAt
		a.TrafficLeft = info.TrafficLeft
		a.Label = info.Label
	} else {
		a.Valid = nil
		a.ExpiresAt = nil
		a.TrafficLeft = nil
		a.Label = ""
	}
	return a, nil
}

func (f *fakeAccounts) List() []*account.Account {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*account.Account, 0, len(f.accounts))
	for _, a := range f.accounts {
		out = append(out, a)
	}
	return out
}

func (f *fakeAccounts) Get(id string) (*account.Account, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[id]
	return a, ok
}

func (f *fakeAccounts) SetEnabled(id string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[id]
	if !ok {
		return account.ErrNotFound
	}
	a.Enabled = enabled
	return nil
}

func (f *fakeAccounts) Delete(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.accounts[id]; !ok {
		return account.ErrNotFound
	}
	delete(f.accounts, id)
	return nil
}

// fakeSettings is a minimal SettingsService for tests: not persisted,
// no validation beyond what settings.Manager itself already covers
// (tested there) — just enough to exercise handlers that read/write
// through it.
type fakeSettings struct {
	mu  sync.Mutex
	cur settings.Settings
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{cur: settings.Settings{
		ParallelFiles:    3,
		ParallelChunks:   4,
		RetryMax:         5,
		AutoStartImports: false,
		DefaultPriority:  "normal",
		LogLevel:         "info",
	}}
}

// fakeStats is a canned StatsService — the API layer only ever reads from
// it, so unlike fakeSettings there's no mutation path to guard with a
// mutex.
type fakeStats struct {
	session stats.SessionStats
	today   stats.DailyStats
	period  stats.AggregateStats
	alltime stats.AllTimeStats
}

func newFakeStats() *fakeStats {
	return &fakeStats{
		session: stats.SessionStats{ByHoster: map[string]int64{}},
		today:   stats.DailyStats{ByHoster: map[string]int64{}},
		period:  stats.AggregateStats{ByHoster: map[string]int64{}},
		alltime: stats.AllTimeStats{ByHoster: map[string]int64{}},
	}
}

func (f *fakeStats) Session() stats.SessionStats { return f.session }
func (f *fakeStats) Today() stats.DailyStats     { return f.today }
func (f *fakeStats) AllTime() stats.AllTimeStats { return f.alltime }

func (f *fakeStats) Period(days int) stats.AggregateStats {
	p := f.period
	p.Days = days
	return p
}

func (f *fakeSettings) Get() settings.Settings {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cur
}

func (f *fakeSettings) Apply(u settings.Update) (settings.Settings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u.ParallelFiles != nil {
		f.cur.ParallelFiles = *u.ParallelFiles
	}
	if u.ParallelChunks != nil {
		f.cur.ParallelChunks = *u.ParallelChunks
	}
	if u.BandwidthLimit != nil {
		f.cur.BandwidthLimit = *u.BandwidthLimit
	}
	if u.RetryMax != nil {
		f.cur.RetryMax = *u.RetryMax
	}
	if u.AutoStartImports != nil {
		f.cur.AutoStartImports = *u.AutoStartImports
	}
	if u.DefaultPriority != nil {
		f.cur.DefaultPriority = *u.DefaultPriority
	}
	if u.LogLevel != nil {
		f.cur.LogLevel = *u.LogLevel
	}
	return f.cur, nil
}

// ---- test setup ----

const testPassword = "correct horse battery staple"

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// defaultTestRegistry mirrors production registration: directhttp (no
// account concept), rapidgator (credentialed, no AccountValidator, like
// the real stub), realdebrid (credentialed and validating).
func defaultTestRegistry() *plugins.Registry {
	return plugins.NewRegistry(
		&fakeUncredentialedHoster{name: "directhttp"},
		&fakeCredentialedHoster{name: "rapidgator"},
		&fakeValidatingHoster{fakeCredentialedHoster: fakeCredentialedHoster{name: "realdebrid"}},
	)
}

func newTestServer(t *testing.T, mutateCfg func(*Config)) (*Server, *fakeQueue) {
	t.Helper()
	return newTestServerWithRegistry(t, defaultTestRegistry(), mutateCfg)
}

func newTestServerWithRegistry(t *testing.T, reg *plugins.Registry, mutateCfg func(*Config)) (*Server, *fakeQueue) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	q := newFakeQueue()
	acc := newFakeAccounts()
	cfg := Config{
		Host:              "127.0.0.1",
		JWTSecret:         "test-jwt-secret-at-least-32-bytes-long",
		AdminPasswordHash: string(hash),
		TokenTTL:          time.Hour,
		RateLimitRPM:      6000,
		RateLimitBurst:    1000,
		DLCEnabled:        false,
		ConfigView:        map[string]string{"redacted": "true"},
	}
	if mutateCfg != nil {
		mutateCfg(&cfg)
	}
	return New(cfg, q, acc, reg, newFakeSettings(), newFakeStats(), nil, nil, discardLogger()), q
}

func loginAndGetToken(t *testing.T, srv *Server) string {
	t.Helper()
	body, _ := json.Marshal(loginRequest{Username: "admin", Password: testPassword})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login failed: status=%d body=%s", w.Code, w.Body.String())
	}
	var resp loginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Token
}

func authedRequest(method, path string, body []byte, token string) *http.Request {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// ---- auth tests ----

func TestLogin_WrongPassword(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	body, _ := json.Marshal(loginRequest{Username: "admin", Password: "wrong"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestLogin_WrongUsername(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	body, _ := json.Marshal(loginRequest{Username: "root", Password: testPassword})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRefreshToken_IssuesFreshToken(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	req := authedRequest(http.MethodPost, "/api/v1/auth/refresh", nil, token)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp loginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" {
		t.Error("expected a non-empty refreshed token")
	}

	// The refreshed token must itself be usable against a protected
	// endpoint — not just well-formed.
	req2 := authedRequest(http.MethodGet, "/api/v1/queue", nil, resp.Token)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("refreshed token rejected: status = %d", w2.Code)
	}
}

func TestRefreshToken_RequiresValidToken(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (an invalid/expired token must not be refreshable)", w.Code)
	}
}

func TestRefreshToken_MissingToken(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestProtectedEndpoint_MissingToken(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/queue", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestProtectedEndpoint_InvalidToken(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/queue", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestProtectedEndpoint_ExpiredToken(t *testing.T) {
	srv, _ := newTestServer(t, nil)

	// New() defaults a non-positive TokenTTL back to a sane value, so an
	// expired token can't be produced by configuring the server with a
	// negative TTL; mint one directly with the same secret instead.
	now := time.Now()
	c := claims{RegisteredClaims: jwt.RegisteredClaims{
		Subject:   adminSubject,
		IssuedAt:  jwt.NewNumericDate(now.Add(-2 * time.Hour)),
		ExpiresAt: jwt.NewNumericDate(now.Add(-time.Hour)),
	}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(srv.cfg.JWTSecret))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/queue", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// ---- queue tests ----

func TestAddLink_Success(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(addLinkRequest{URL: "http://example.com/f", Priority: "high"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/links", body, token))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if len(fq.List()) != 1 {
		t.Errorf("expected 1 item queued, got %d", len(fq.List()))
	}
}

func TestAddLink_MissingURL(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(addLinkRequest{Priority: "normal"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/links", body, token))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestAddLink_InvalidPriority(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(addLinkRequest{URL: "http://example.com/f", Priority: "urgent"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/links", body, token))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestQueueLifecycle_PauseResumeRetryCancel(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	it, _ := fq.AddLink("http://example.com/f", queue.AddOptions{})

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/queue/"+it.ID+"/pause", nil, token))
	if w.Code != http.StatusOK {
		t.Fatalf("pause: status = %d body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/queue/"+it.ID+"/resume", nil, token))
	if w.Code != http.StatusOK {
		t.Fatalf("resume: status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/queue/"+it.ID+"/retry", nil, token))
	if w.Code != http.StatusOK {
		t.Fatalf("retry: status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodDelete, "/api/v1/queue/"+it.ID, nil, token))
	if w.Code != http.StatusNoContent {
		t.Fatalf("cancel: status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/queue/"+it.ID, nil, token))
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 after cancel, got %d", w.Code)
	}
}

// TestBulkCancel_DeletesAllListedItemsInOneRequest is the regression test
// for the bug this endpoint fixes: "Clear done"/bulk-cancel used to fire
// one DELETE per item in parallel, which could trip the API's per-client
// rate limiter on a large batch. A single bulk request should delete
// every listed id and never come back partially failed when every id is
// valid.
func TestBulkCancel_DeletesAllListedItemsInOneRequest(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	ids := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		it, err := fq.AddLink(fmt.Sprintf("http://example.com/f%d", i), queue.AddOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, it.ID)
	}

	body, _ := json.Marshal(bulkCancelRequest{IDs: ids})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodDelete, "/api/v1/queue", body, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	var resp bulkCancelResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Deleted) != 20 {
		t.Errorf("len(Deleted) = %d, want 20", len(resp.Deleted))
	}
	if len(resp.Failed) != 0 {
		t.Errorf("len(Failed) = %d, want 0", len(resp.Failed))
	}

	for _, id := range ids {
		if _, ok := fq.Get(id); ok {
			t.Errorf("item %s still present after bulk cancel", id)
		}
	}
}

func TestBulkCancel_UnknownIDReportedAsFailedNotWholeRequestError(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	it, _ := fq.AddLink("http://example.com/f", queue.AddOptions{})

	body, _ := json.Marshal(bulkCancelRequest{IDs: []string{it.ID, "does-not-exist"}})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodDelete, "/api/v1/queue", body, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	var resp bulkCancelResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Deleted) != 1 || resp.Deleted[0] != it.ID {
		t.Errorf("Deleted = %v, want [%s]", resp.Deleted, it.ID)
	}
	if len(resp.Failed) != 1 || resp.Failed[0] != "does-not-exist" {
		t.Errorf("Failed = %v, want [does-not-exist]", resp.Failed)
	}
}

func TestBulkCancel_EmptyIDsRejected(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(bulkCancelRequest{IDs: []string{}})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodDelete, "/api/v1/queue", body, token))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestRetryExtract_Success(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	it, _ := fq.AddLink("http://example.com/f.zip", queue.AddOptions{})
	fq.mu.Lock()
	it.State = queue.StateDone
	it.ExtractState = queue.ExtractStateExtractFailed
	it.ExtractError = "wrong password"
	fq.mu.Unlock()

	body, _ := json.Marshal(retryExtractRequest{Password: "secret"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/queue/"+it.ID+"/extract", body, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if fq.lastRetryExtractPassword != "secret" {
		t.Errorf("password passed to RetryExtraction = %q, want secret", fq.lastRetryExtractPassword)
	}
	var got queue.Item
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ExtractState != queue.ExtractStateExtracted {
		t.Errorf("returned item extract_state = %q, want extracted", got.ExtractState)
	}
}

func TestRetryExtract_NoBodyIsValid(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	it, _ := fq.AddLink("http://example.com/f.zip", queue.AddOptions{})
	fq.mu.Lock()
	it.State = queue.StateDone
	it.ExtractState = queue.ExtractStateExtractFailed
	fq.mu.Unlock()

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/queue/"+it.ID+"/extract", nil, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if fq.lastRetryExtractPassword != "" {
		t.Errorf("password = %q, want empty for a bodyless request", fq.lastRetryExtractPassword)
	}
}

func TestRetryExtract_NotFound(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/queue/nope/extract", nil, token))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestRetryExtract_NotRetryableReturnsBadRequest(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	// A freshly-added item is StateQueued with no extract_state at all —
	// not retryable.
	it, _ := fq.AddLink("http://example.com/f.zip", queue.AddOptions{})

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/queue/"+it.ID+"/extract", nil, token))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestRetryExtract_ArchiveMissingReturnsBadRequest(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	fq.retryExtractErr = queue.ErrArchiveMissing

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/queue/whatever/extract", nil, token))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestRetryExtract_InvalidJSONBodyRejected(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/queue/whatever/extract", []byte("{not json"), token))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestPause_NotFound(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/queue/nope/pause", nil, token))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestPause_ConflictError(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	fq.pauseErr = errors.New("cannot pause item in state done")

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/queue/whatever/pause", nil, token))
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestPauseAll_PausesQueuedAndDownloading(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	it1, _ := fq.AddLink("http://example.com/1", queue.AddOptions{})
	it2, _ := fq.AddLink("http://example.com/2", queue.AddOptions{})
	fq.items[it2.ID].State = queue.StateDownloading
	itDone, _ := fq.AddLink("http://example.com/3", queue.AddOptions{})
	fq.items[itDone.ID].State = queue.StateDone

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/queue/pause-all", nil, token))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	got1, _ := fq.Get(it1.ID)
	got2, _ := fq.Get(it2.ID)
	gotDone, _ := fq.Get(itDone.ID)
	if got1.State != queue.StatePaused || got2.State != queue.StatePaused {
		t.Errorf("expected queued/downloading items paused, got %s / %s", got1.State, got2.State)
	}
	if gotDone.State != queue.StateDone {
		t.Errorf("expected done item untouched, got %s", gotDone.State)
	}
}

func TestResumeAll_RequeuesPausedOnly(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	it, _ := fq.AddLink("http://example.com/1", queue.AddOptions{})
	fq.items[it.ID].State = queue.StatePaused
	itDone, _ := fq.AddLink("http://example.com/2", queue.AddOptions{})
	fq.items[itDone.ID].State = queue.StateDone

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/queue/resume-all", nil, token))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	got, _ := fq.Get(it.ID)
	gotDone, _ := fq.Get(itDone.ID)
	if got.State != queue.StateQueued {
		t.Errorf("expected paused item requeued, got %s", got.State)
	}
	if gotDone.State != queue.StateDone {
		t.Errorf("expected done item untouched, got %s", gotDone.State)
	}
}

func TestPatchQueueItem_UpdatesPriority(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	it, _ := fq.AddLink("http://example.com/f", queue.AddOptions{Priority: queue.PriorityNormal})

	body, _ := json.Marshal(patchQueueItemRequest{Priority: "high"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPatch, "/api/v1/queue/"+it.ID, body, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	got, _ := fq.Get(it.ID)
	if got.Priority != queue.PriorityHigh {
		t.Errorf("Priority = %v, want high", got.Priority)
	}
}

func TestPatchQueueItem_InvalidPriority(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	it, _ := fq.AddLink("http://example.com/f", queue.AddOptions{})

	body, _ := json.Marshal(patchQueueItemRequest{Priority: "urgent"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPatch, "/api/v1/queue/"+it.ID, body, token))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestPatchQueueItem_NotFound(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	body, _ := json.Marshal(patchQueueItemRequest{Priority: "high"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPatch, "/api/v1/queue/nope", body, token))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestPatchPackage_UpdatesPath(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	pkg, _, _ := fq.AddPackage("Movies", []string{"http://example.com/f"}, queue.AddOptions{})

	path := "movies/2026"
	body, _ := json.Marshal(patchPackageRequest{Path: &path})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPatch, "/api/v1/packages/"+pkg.ID, body, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	var got queue.Package
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Path != "movies/2026" {
		t.Errorf("Path = %q, want movies/2026", got.Path)
	}
}

func TestPatchPackage_MissingPathField(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	pkg, _, _ := fq.AddPackage("Movies", []string{"http://example.com/f"}, queue.AddOptions{})

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPatch, "/api/v1/packages/"+pkg.ID, []byte(`{}`), token))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestPatchPackage_NotFound(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)
	path := "x"
	body, _ := json.Marshal(patchPackageRequest{Path: &path})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPatch, "/api/v1/packages/nope", body, token))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// ---- settings tests ----

func TestGetSettings_ReturnsCurrentValues(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/settings", nil, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var got settings.Settings
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ParallelFiles != 3 {
		t.Errorf("ParallelFiles = %d, want 3 (from fakeSettings default)", got.ParallelFiles)
	}
}

func TestUpdateSettings_PartialUpdate(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(settings.Update{RetryMax: intPtr(9)})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/settings", body, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var got settings.Settings
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RetryMax != 9 {
		t.Errorf("RetryMax = %d, want 9", got.RetryMax)
	}
	if got.ParallelFiles != 3 {
		t.Errorf("ParallelFiles = %d, want unchanged at 3", got.ParallelFiles)
	}
}

func TestAddLink_UsesDefaultPriorityFromSettingsWhenUnset(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(addLinkRequest{URL: "http://example.com/f"}) // no Priority
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/links", body, token))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	items := fq.List()
	if len(items) != 1 || items[0].Priority != queue.PriorityNormal {
		t.Errorf("expected 1 item with default (normal) priority, got %+v", items)
	}
}

func intPtr(n int) *int { return &n }

// ---- account tests ----

func TestAccountLifecycle(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(addAccountRequest{HosterKey: "rapidgator", Username: "alice", Password: "s3cret"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts", body, token))
	if w.Code != http.StatusCreated {
		t.Fatalf("add account: status = %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "s3cret") {
		t.Error("account response leaked the plaintext password")
	}
	var acc account.Account
	if err := json.Unmarshal(w.Body.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}

	body, _ = json.Marshal(setEnabledRequest{Enabled: false})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts/"+acc.ID+"/enabled", body, token))
	if w.Code != http.StatusOK {
		t.Fatalf("set enabled: status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodDelete, "/api/v1/accounts/"+acc.ID, nil, token))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d", w.Code)
	}
}

func TestAddAccount_RejectsUnknownHosterKey(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(addAccountRequest{HosterKey: "not-a-real-plugin", Username: "alice", Password: "s3cret"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts", body, token))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestAddAccount_RejectsUncredentialedHosterKey(t *testing.T) {
	// directhttp is registered but has no Credentialed() method at all
	// (like the real plugin) — it must not be a valid hoster_key.
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(addAccountRequest{HosterKey: "directhttp", Username: "alice", Password: "s3cret"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts", body, token))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestListPlugins_OnlyReturnsCredentialedPlugins(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/plugins", nil, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var got []pluginInfo
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d plugins, want 2 (rapidgator, realdebrid); body=%s", len(got), w.Body.String())
	}
	keys := map[string]bool{}
	for _, p := range got {
		keys[p.Key] = true
		if p.Name != p.Key {
			t.Errorf("plugin %q: Name = %q, want same as Key (fakeCredentialedHoster has no DisplayNamer)", p.Key, p.Name)
		}
	}
	if !keys["rapidgator"] || !keys["realdebrid"] {
		t.Errorf("got keys %v, want rapidgator and realdebrid", keys)
	}
	if keys["directhttp"] {
		t.Error("directhttp (uncredentialed) should not appear in the plugin list")
	}
}

// ---- account validation ----

func TestAddAccount_InlineValidation_Success(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(addAccountRequest{HosterKey: "realdebrid", Username: "alice", Password: "s3cret"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts", body, token))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	var acc account.Account
	if err := json.Unmarshal(w.Body.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Valid == nil || !*acc.Valid {
		t.Errorf("Valid = %v, want true", acc.Valid)
	}
	if acc.Label != "Premium" {
		t.Errorf("Label = %q, want Premium", acc.Label)
	}
	if acc.ValidatedAt == nil {
		t.Error("expected ValidatedAt to be set")
	}
}

func TestAddAccount_InlineValidation_HosterReportsInvalid(t *testing.T) {
	reg := plugins.NewRegistry(&fakeValidatingHoster{
		fakeCredentialedHoster: fakeCredentialedHoster{name: "realdebrid"},
		validateFn: func(acc *plugins.Account) (*plugins.AccountInfo, error) {
			return &plugins.AccountInfo{Valid: false, Label: "Invalid API token"}, nil
		},
	})
	srv, _ := newTestServerWithRegistry(t, reg, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(addAccountRequest{HosterKey: "realdebrid", Username: "alice", Password: "bad-token"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts", body, token))
	if w.Code != http.StatusCreated {
		t.Fatalf("account creation should still succeed even when validation reports invalid: status = %d, body=%s", w.Code, w.Body.String())
	}

	var acc account.Account
	if err := json.Unmarshal(w.Body.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Valid == nil || *acc.Valid {
		t.Errorf("Valid = %v, want false", acc.Valid)
	}
	if acc.Label != "Invalid API token" {
		t.Errorf("Label = %q, want %q", acc.Label, "Invalid API token")
	}
}

func TestAddAccount_InlineValidation_HosterCallFails_LeavesUnknown(t *testing.T) {
	reg := plugins.NewRegistry(&fakeValidatingHoster{
		fakeCredentialedHoster: fakeCredentialedHoster{name: "realdebrid"},
		validateFn: func(acc *plugins.Account) (*plugins.AccountInfo, error) {
			return nil, errors.New("network error")
		},
	})
	srv, _ := newTestServerWithRegistry(t, reg, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(addAccountRequest{HosterKey: "realdebrid", Username: "alice", Password: "s3cret"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts", body, token))
	if w.Code != http.StatusCreated {
		t.Fatalf("account creation should still succeed when the hoster call itself fails: status = %d, body=%s", w.Code, w.Body.String())
	}

	var acc account.Account
	if err := json.Unmarshal(w.Body.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Valid != nil {
		t.Errorf("Valid = %v, want nil (unknown) when the hoster call itself errors", *acc.Valid)
	}
}

func TestAddAccount_InlineValidation_UnsupportedPluginLeavesUnknown(t *testing.T) {
	// rapidgator doesn't implement AccountValidator, matching the real
	// stub — the account is created but valid stays null.
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(addAccountRequest{HosterKey: "rapidgator", Username: "alice", Password: "s3cret"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts", body, token))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"valid":null`) {
		t.Errorf("expected valid:null in response, got %s", w.Body.String())
	}
}

func TestValidateAccount_Recheck_UpdatesState(t *testing.T) {
	var callCount int
	reg := plugins.NewRegistry(&fakeValidatingHoster{
		fakeCredentialedHoster: fakeCredentialedHoster{name: "realdebrid"},
		validateFn: func(acc *plugins.Account) (*plugins.AccountInfo, error) {
			callCount++
			if callCount == 1 {
				return &plugins.AccountInfo{Valid: false, Label: "Invalid API token"}, nil
			}
			return &plugins.AccountInfo{Valid: true, Label: "Premium"}, nil
		},
	})
	srv, _ := newTestServerWithRegistry(t, reg, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(addAccountRequest{HosterKey: "realdebrid", Username: "alice", Password: "s3cret"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts", body, token))
	var acc account.Account
	if err := json.Unmarshal(w.Body.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Valid == nil || *acc.Valid {
		t.Fatalf("setup: expected the initial (inline) validation to report invalid, got %+v", acc)
	}

	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts/"+acc.ID+"/validate", nil, token))
	if w.Code != http.StatusOK {
		t.Fatalf("validate: status = %d, body=%s", w.Code, w.Body.String())
	}
	var updated account.Account
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Valid == nil || !*updated.Valid {
		t.Errorf("after re-check, Valid = %v, want true", updated.Valid)
	}
	if callCount != 2 {
		t.Errorf("hoster validated %d times, want 2 (once on add, once on re-check)", callCount)
	}
}

func TestValidateAccount_UnsupportedPlugin(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(addAccountRequest{HosterKey: "rapidgator", Username: "alice", Password: "s3cret"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts", body, token))
	var acc account.Account
	_ = json.Unmarshal(w.Body.Bytes(), &acc)

	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts/"+acc.ID+"/validate", nil, token))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestValidateAccount_NotFound(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts/does-not-exist/validate", nil, token))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestValidateAccount_HosterCallFails(t *testing.T) {
	reg := plugins.NewRegistry(&fakeValidatingHoster{
		fakeCredentialedHoster: fakeCredentialedHoster{name: "realdebrid"},
		validateFn: func(acc *plugins.Account) (*plugins.AccountInfo, error) {
			return nil, errors.New("network error")
		},
	})
	srv, _ := newTestServerWithRegistry(t, reg, nil)
	token := loginAndGetToken(t, srv)

	body, _ := json.Marshal(addAccountRequest{HosterKey: "realdebrid", Username: "alice", Password: "s3cret"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts", body, token))
	var acc account.Account
	_ = json.Unmarshal(w.Body.Bytes(), &acc)

	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/accounts/"+acc.ID+"/validate", nil, token))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (explicit re-check surfaces hoster-call failures); body=%s", w.Code, w.Body.String())
	}
}

// ---- config/stats ----

func TestGetConfig_NoSecretsLeaked(t *testing.T) {
	srv, _ := newTestServer(t, func(c *Config) {
		c.ConfigView = map[string]any{"server": map[string]any{"port": 9667}}
	})
	token := loginAndGetToken(t, srv)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/config", nil, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "jwt_secret") || strings.Contains(w.Body.String(), srv.cfg.JWTSecret) {
		t.Error("config response leaked a secret")
	}
}

func TestStats(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/stats", nil, token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp statsAPIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Week.Days != daysInWeek {
		t.Errorf("Week.Days = %d, want %d", resp.Week.Days, daysInWeek)
	}
	if resp.Month.Days != daysInMonth {
		t.Errorf("Month.Days = %d, want %d", resp.Month.Days, daysInMonth)
	}
	if resp.Year.Days != daysInYear {
		t.Errorf("Year.Days = %d, want %d", resp.Year.Days, daysInYear)
	}
}

// ---- import ----

func multipartBody(t *testing.T, filename string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

func TestImport_PlaintextLinks(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	body, contentType := multipartBody(t, "links.txt", []byte("http://example.com/a\nhttp://example.com/b\n"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/import", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if len(fq.packages) != 1 {
		t.Errorf("expected 1 package created, got %d", len(fq.packages))
	}
}

// TestLinkInputs_KeepsPerFileFilenamesAndSizes is the regression test
// for a real gap found alongside the sequential-part-ordering feature:
// handleImport used to only pull f.URL out of each dlc.Package's files,
// silently dropping f.Filename — which meant a DLC-imported multi-part
// RAR set (the common real-world case a .dlc container exists for)
// could never be recognized as one by Config.SequentialParts until its
// download actually started, by which point there was nothing left for
// sequential ordering to decide. See queue.AddOptions.Filenames.
func TestLinkInputs_KeepsPerFileFilenamesAndSizes(t *testing.T) {
	pkg := dlc.Package{
		Files: []dlc.File{
			{URL: "https://rapidgator.net/file/a.html", Filename: "movie.part1.rar", Size: 100},
			{URL: "https://rapidgator.net/file/b.html", Filename: "movie.part2.rar"},
			{URL: "", Filename: "skipped-because-no-url"},
			{URL: "https://rapidgator.net/file/c.html", Filename: ""},
		},
	}
	got := linkInputs(pkg)
	want := []queue.LinkInput{
		{URL: "https://rapidgator.net/file/a.html", Filename: "movie.part1.rar", Size: 100},
		{URL: "https://rapidgator.net/file/b.html", Filename: "movie.part2.rar"},
		{URL: "https://rapidgator.net/file/c.html"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestImport_DLCDisabled(t *testing.T) {
	srv, _ := newTestServer(t, nil) // DLCEnabled: false by default in fixture
	token := loginAndGetToken(t, srv)

	body, contentType := multipartBody(t, "links.dlc", []byte("irrelevant"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/import", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// ---- CORS ----

func TestCORS_DisallowedOriginGetsNoHeaders(t *testing.T) {
	srv, _ := newTestServer(t, func(c *Config) { c.CORSAllowedOrigins = []string{"https://allowed.example.com"} })
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("expected no CORS header for disallowed origin")
	}
}

func TestCORS_AllowedOriginGetsHeader(t *testing.T) {
	srv, _ := newTestServer(t, func(c *Config) { c.CORSAllowedOrigins = []string{"https://allowed.example.com"} })
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://allowed.example.com")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Header().Get("Access-Control-Allow-Origin") != "https://allowed.example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the allowed origin", w.Header().Get("Access-Control-Allow-Origin"))
	}
}

// ---- security headers ----

func TestSecurityHeadersPresent(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy", "Strict-Transport-Security"} {
		if w.Header().Get(h) == "" {
			t.Errorf("missing security header %s", h)
		}
	}
}

// ---- rate limiting ----

func TestRateLimit_ExceedsQuota(t *testing.T) {
	srv, _ := newTestServer(t, func(c *Config) {
		c.RateLimitRPM = 60 // 1/sec
		c.RateLimitBurst = 1
	})

	var last *httptest.ResponseRecorder
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.RemoteAddr = "10.0.0.1:1"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		last = w
	}
	if last.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after exceeding burst, got %d", last.Code)
	}
}

// ---- websocket ----

func TestWebSocket_ReceivesQueueEvents(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	token := loginAndGetToken(t, srv)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws?" + url.Values{"token": {token}}.Encode()
	conn, dialResp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if dialResp != nil {
		defer dialResp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Give the server a moment to register the subscription before
	// publishing, since Subscribe happens inside the handler goroutine.
	time.Sleep(50 * time.Millisecond)
	fq.publish(queue.Event{Type: queue.EventItemUpdated, Item: &queue.Item{ID: "item-1", State: queue.StateDownloading}})

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var ev queue.Event
	if err := conn.ReadJSON(&ev); err != nil {
		t.Fatalf("read: %v", err)
	}
	if ev.Item == nil || ev.Item.ID != "item-1" {
		t.Errorf("unexpected event: %+v", ev)
	}
}

func TestWebSocket_RejectsMissingToken(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected dial to fail without a token")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}
