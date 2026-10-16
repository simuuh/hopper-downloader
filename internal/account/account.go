// Package account manages hoster credentials: it persists them AES-256-GCM
// encrypted at rest and hands back plaintext plugins.Account values only
// in memory, for the duration of a single plugin call.
package account

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"go.etcd.io/bbolt"

	"github.com/simuuh/hopper-downloader/plugins"
)

// ErrNotFound is returned when an account ID doesn't exist.
var ErrNotFound = errors.New("account: not found")

var bucketName = []byte("accounts")

// Account is the public, non-secret view of a stored hoster credential:
// safe to return from the REST API as-is.
type Account struct {
	ID        string    `json:"id"`
	HosterKey string    `json:"hoster_key"`
	Username  string    `json:"username"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Validation state, cached from the hoster's own API (see
	// plugins.AccountValidator) rather than re-checked on every read.
	// Valid is a pointer, and deliberately not omitempty, so "never
	// validated" / "hoster plugin doesn't support validation" (nil,
	// serializes as JSON null) is distinguishable from "validated and
	// found invalid" (false) — a plain bool can't represent that third
	// state. The rest are omitempty since they're meaningless while
	// Valid is nil.
	Valid       *bool      `json:"valid"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	TrafficLeft *int64     `json:"traffic_left,omitempty"`
	Label       string     `json:"label,omitempty"`
	ValidatedAt *time.Time `json:"validated_at,omitempty"`

	// CooldownUntil is set while the account is skipped by AccountFor
	// after a download reported it unusable (see ReportFailure). In-memory
	// only — filled into the public view by List/Get, never persisted.
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
}

// secret is the part of an account that's encrypted at rest.
type secret struct {
	Password string            `json:"password"`
	Extra    map[string]string `json:"extra,omitempty"`
}

// record is what's actually persisted: public metadata in the clear
// (needed to list/search accounts without decrypting every one) plus the
// AES-256-GCM sealed secret.
type record struct {
	Account
	Ciphertext []byte `json:"ciphertext"`
}

// Manager stores and retrieves hoster accounts. It is the single owner of
// the encryption key in memory; callers never see it.
type Manager struct {
	db  *bbolt.DB
	gcm cipher.AEAD
	log *slog.Logger

	mu       sync.RWMutex
	accounts map[string]*record
	// cooldown maps account id -> time until which AccountFor skips it.
	cooldown map[string]time.Time
}

// failureCooldown is how long an account that a download reported as
// unusable (expired, out of traffic, rejected) is skipped in favor of the
// hoster's other accounts. Short enough that a daily traffic reset or a
// renewed premium is picked up again soon without manual action; the 12h
// background re-validation and a manual "Re-check" clear it earlier once
// the hoster confirms the account is fine.
const failureCooldown = 30 * time.Minute

// New opens (creating if necessary) the accounts bucket in db and derives
// an AES-256-GCM cipher from encryptionKey, which must be exactly 32
// bytes. It never logs or otherwise exposes encryptionKey or any
// decrypted secret.
func New(db *bbolt.DB, encryptionKey string, log *slog.Logger) (*Manager, error) {
	if log == nil {
		return nil, fmt.Errorf("account: logger is required")
	}
	gcm, err := newAEAD(encryptionKey)
	if err != nil {
		return nil, err
	}

	err = db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("account: init bucket: %w", err)
	}

	m := &Manager{db: db, gcm: gcm, log: log, accounts: make(map[string]*record), cooldown: make(map[string]time.Time)}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) load() error {
	return m.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketName).ForEach(func(_, v []byte) error {
			var r record
			if err := json.Unmarshal(v, &r); err != nil {
				return fmt.Errorf("account: unmarshal record: %w", err)
			}
			m.accounts[r.ID] = &r
			return nil
		})
	})
}

func (m *Manager) persist(r *record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("account: marshal record: %w", err)
	}
	return m.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketName).Put([]byte(r.ID), data)
	})
}

// Add creates a new account. password and extra are encrypted before
// touching disk; only username and hosterKey are stored in the clear.
func (m *Manager) Add(hosterKey, username, password string, extra map[string]string) (*Account, error) {
	id := newID()
	sec, err := json.Marshal(secret{Password: password, Extra: extra})
	if err != nil {
		return nil, fmt.Errorf("account: marshal secret: %w", err)
	}
	ciphertext, err := seal(m.gcm, sec)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	r := &record{
		Account: Account{
			ID:        id,
			HosterKey: hosterKey,
			Username:  username,
			Enabled:   true,
			CreatedAt: now,
			UpdatedAt: now,
		},
		Ciphertext: ciphertext,
	}

	m.mu.Lock()
	m.accounts[id] = r
	m.mu.Unlock()

	if err := m.persist(r); err != nil {
		return nil, err
	}
	pub := r.Account
	return &pub, nil
}

// SetEnabled toggles whether an account is eligible for AccountFor.
func (m *Manager) SetEnabled(id string, enabled bool) error {
	m.mu.Lock()
	r, ok := m.accounts[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	r.Enabled = enabled
	r.UpdatedAt = time.Now()
	if enabled {
		delete(m.cooldown, id)
	}
	cp := *r
	m.mu.Unlock()
	return m.persist(&cp)
}

// Delete removes an account permanently.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	if _, ok := m.accounts[id]; !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	delete(m.accounts, id)
	delete(m.cooldown, id)
	m.mu.Unlock()

	return m.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketName).Delete([]byte(id))
	})
}

// List returns the public (non-secret) view of every stored account.
func (m *Manager) List() []*Account {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Account, 0, len(m.accounts))
	for _, r := range m.accounts {
		out = append(out, m.publicLocked(r))
	}
	return out
}

// publicLocked returns r's public view with the in-memory cooldown filled
// in. Caller holds m.mu (read or write).
func (m *Manager) publicLocked(r *record) *Account {
	pub := r.Account
	if until, ok := m.cooldown[r.ID]; ok && until.After(time.Now()) {
		u := until
		pub.CooldownUntil = &u
	}
	return &pub
}

// Get returns the public (non-secret) view of one account.
func (m *Manager) Get(id string) (*Account, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.accounts[id]
	if !ok {
		return nil, false
	}
	return m.publicLocked(r), true
}

// AccountFor implements queue.AccountLookup: it decrypts and returns the
// best enabled account for hosterKey, so a hoster with several accounts
// rotates on its own:
//
//   - skipped: accounts known to be invalid, expired, out of traffic, or
//     in cooldown after a download reported them unusable (ReportFailure);
//   - preferred: confirmed-valid over never-validated, then the most
//     traffic left (unknown/unlimited counts as most), then the oldest.
//
// If every enabled account is skipped, it still returns one (one not in
// cooldown if possible) rather than none: plugins' Match and the queue's
// plugin priority treat "has an account" as "is configured", and the
// hoster's own error is a clearer message than "no account".
func (m *Manager) AccountFor(hosterKey string) (*plugins.Account, bool) {
	now := time.Now()
	m.mu.RLock()
	var candidates []*record
	for _, r := range m.accounts {
		if r.HosterKey == hosterKey && r.Enabled {
			candidates = append(candidates, r)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return better(candidates[i], candidates[j]) })
	var match, fallback *record
	for _, r := range candidates {
		cooling := m.cooldown[r.ID].After(now)
		if !cooling && usable(r, now) {
			match = r
			break
		}
		if fallback == nil || (!cooling && m.cooldown[fallback.ID].After(now)) {
			fallback = r
		}
	}
	m.mu.RUnlock()
	if match == nil {
		match = fallback
	}
	if match == nil {
		return nil, false
	}
	acc, err := m.decrypt(match)
	if err != nil {
		return nil, false
	}
	return acc, true
}

// usable reports whether r's cached validation state allows using it.
// Unknown (never validated) counts as usable.
func usable(r *record, now time.Time) bool {
	if r.Valid != nil && !*r.Valid {
		return false
	}
	if r.ExpiresAt != nil && r.ExpiresAt.Before(now) {
		return false
	}
	if r.TrafficLeft != nil && *r.TrafficLeft <= 0 {
		return false
	}
	return true
}

// better orders AccountFor's candidates: confirmed-valid first, then most
// traffic left (nil = unknown/unlimited sorts first), then oldest, then id.
func better(a, b *record) bool {
	av, bv := a.Valid != nil && *a.Valid, b.Valid != nil && *b.Valid
	if av != bv {
		return av
	}
	if (a.TrafficLeft == nil) != (b.TrafficLeft == nil) {
		return a.TrafficLeft == nil
	}
	if a.TrafficLeft != nil && *a.TrafficLeft != *b.TrafficLeft {
		return *a.TrafficLeft > *b.TrafficLeft
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

// ReportFailure puts account id into a short cooldown (failureCooldown)
// after a download found it unusable — premium expired, traffic used up,
// credentials rejected — so AccountFor hands out the hoster's next
// account. It reports whether id was known. queue.Manager calls it via
// its AccountFailureReporter interface.
func (m *Manager) ReportFailure(id string) bool {
	m.mu.Lock()
	r, ok := m.accounts[id]
	hoster := ""
	if ok {
		m.cooldown[id] = time.Now().Add(failureCooldown)
		hoster = r.HosterKey
	}
	m.mu.Unlock()
	if ok {
		m.log.Warn("account: rejected by hoster, rotating to next account", "account_id", id, "hoster", hoster, "cooldown", failureCooldown)
	}
	return ok
}

// Credentials decrypts and returns the plaintext credentials for the
// exact account id, unlike AccountFor (which picks "the" enabled account
// for a hoster key — Phase 1 has no per-download account selection).
// Validation flows already know which specific account they're
// re-checking, and don't filter on Enabled: re-validating a disabled
// account is still useful, e.g. to decide whether it's worth re-enabling.
func (m *Manager) Credentials(id string) (*plugins.Account, error) {
	m.mu.RLock()
	r, ok := m.accounts[id]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	return m.decrypt(r)
}

func (m *Manager) decrypt(r *record) (*plugins.Account, error) {
	plaintext, err := open(m.gcm, r.Ciphertext)
	if err != nil {
		m.log.Error("account: decrypt failed", "account_id", r.ID, "error", err)
		return nil, fmt.Errorf("account: decrypt failed")
	}
	var sec secret
	if err := json.Unmarshal(plaintext, &sec); err != nil {
		m.log.Error("account: unmarshal secret failed", "account_id", r.ID, "error", err)
		return nil, fmt.Errorf("account: unmarshal secret failed")
	}
	return &plugins.Account{ID: r.ID, Username: r.Username, Password: sec.Password, Extra: sec.Extra}, nil
}

// SetValidation persists the cached result of validating id's credentials
// against the hoster's own API (see plugins.AccountValidator), so
// List/Get can show last-known status without re-validating on every
// read. info nil means "the hoster call itself failed" (network error,
// unexpected response) as opposed to a definite valid/invalid answer —
// Valid is left nil (unknown) in that case too, same as before any
// validation was ever attempted.
func (m *Manager) SetValidation(id string, info *plugins.AccountInfo) (*Account, error) {
	m.mu.Lock()
	r, ok := m.accounts[id]
	if !ok {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	now := time.Now()
	r.ValidatedAt = &now
	r.UpdatedAt = now
	if info != nil {
		valid := info.Valid
		r.Valid = &valid
		if valid {
			delete(m.cooldown, id)
		}
		r.ExpiresAt = info.ExpiresAt
		r.TrafficLeft = info.TrafficLeft
		r.Label = info.Label
	} else {
		r.Valid = nil
		r.ExpiresAt = nil
		r.TrafficLeft = nil
		r.Label = ""
	}
	cp := *r
	m.mu.Unlock()

	if err := m.persist(&cp); err != nil {
		return nil, err
	}
	pub := cp.Account
	return &pub, nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// UpdateCredentials replaces account id's username and secret — e.g.
// after renewing premium, when the hoster issued a new API token. An
// empty username or password keeps the stored one; extra replaces the
// stored extra map only when non-nil. The cached validation result is
// cleared since it described the old credentials.
func (m *Manager) UpdateCredentials(id, username, password string, extra map[string]string) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.accounts[id]
	if !ok {
		return nil, ErrNotFound
	}
	cur, err := m.decrypt(r)
	if err != nil {
		return nil, err
	}
	if password == "" {
		password = cur.Password
	}
	if extra == nil {
		extra = cur.Extra
	}
	sec, err := json.Marshal(secret{Password: password, Extra: extra})
	if err != nil {
		return nil, fmt.Errorf("account: marshal secret: %w", err)
	}
	ciphertext, err := seal(m.gcm, sec)
	if err != nil {
		return nil, err
	}

	updated := *r
	updated.Ciphertext = ciphertext
	if username != "" {
		updated.Username = username
	}
	updated.Valid, updated.ExpiresAt, updated.TrafficLeft, updated.Label, updated.ValidatedAt = nil, nil, nil, "", nil
	updated.UpdatedAt = time.Now()
	if err := m.persist(&updated); err != nil {
		return nil, err
	}
	m.accounts[id] = &updated
	delete(m.cooldown, id)
	pub := updated.Account
	return &pub, nil
}
