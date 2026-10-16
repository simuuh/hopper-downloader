package onefichier

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/simuuh/hopper-downloader/plugins"
)

func TestValidateAccount_Success(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/info.cgi" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		checkAuth(t, r, "test-token")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"email":       "user@example.com",
			"premium_end": "2026-12-31 00:00:00",
		})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), &fakeTokens{token: "test-token"})

	info, err := p.ValidateAccount(&plugins.Account{Extra: map[string]string{"api_key": "test-token"}})
	if err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if !info.Valid {
		t.Error("expected Valid = true")
	}
	if info.ExpiresAt == nil || info.ExpiresAt.Format("2006-01-02") != "2026-12-31" {
		t.Errorf("ExpiresAt = %v, want 2026-12-31", info.ExpiresAt)
	}
	if info.Label != "Premium until 2026-12-31" {
		t.Errorf("Label = %q, want %q", info.Label, "Premium until 2026-12-31")
	}
}

func TestValidateAccount_SuccessWithoutRecognizedExpiryField(t *testing.T) {
	// If the response doesn't contain any of premiumExpiryFields (the
	// actual field name isn't independently confirmed - see this
	// package's doc comment), ValidateAccount must still report
	// Valid: true from the call succeeding at all, just without a date.
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"email": "user@example.com"})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), &fakeTokens{token: "test-token"})

	info, err := p.ValidateAccount(&plugins.Account{Extra: map[string]string{"api_key": "test-token"}})
	if err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if !info.Valid {
		t.Error("expected Valid = true")
	}
	if info.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil", info.ExpiresAt)
	}
	if info.Label != "Premium" {
		t.Errorf("Label = %q, want generic \"Premium\"", info.Label)
	}
}

func TestValidateAccount_InvalidToken_HTTPStatus(t *testing.T) {
	p := newTestPlugin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}), &fakeTokens{token: "bad-token"})

	info, err := p.ValidateAccount(&plugins.Account{Extra: map[string]string{"api_key": "bad-token"}})
	if err != nil {
		t.Fatalf("ValidateAccount returned an error, want a (Valid: false) result: %v", err)
	}
	if info.Valid {
		t.Error("expected Valid = false")
	}
	if info.Label != "Invalid API key" {
		t.Errorf("Label = %q, want %q", info.Label, "Invalid API key")
	}
}

func TestValidateAccount_InvalidToken_KOStatus(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "KO", "message": "Bad API key"})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), &fakeTokens{token: "bad-token"})

	info, err := p.ValidateAccount(&plugins.Account{Extra: map[string]string{"api_key": "bad-token"}})
	if err != nil {
		t.Fatalf("ValidateAccount returned an error, want a (Valid: false) result: %v", err)
	}
	if info.Valid {
		t.Error("expected Valid = false")
	}
}

func TestValidateAccount_NoTokenConfigured(t *testing.T) {
	p := New(&fakeTokens{}, nil)
	_, err := p.ValidateAccount(nil)
	if err != plugins.ErrAccountRequired {
		t.Errorf("err = %v, want plugins.ErrAccountRequired", err)
	}
}

func TestValidateAccount_FallsBackToPassword(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r, "webui-token")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"email": "user@example.com"})
	}
	p := newTestPlugin(t, http.HandlerFunc(handler), &fakeTokens{password: "webui-token"})

	info, err := p.ValidateAccount(&plugins.Account{Password: "webui-token"})
	if err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}
	if !info.Valid {
		t.Error("expected Valid = true using the password-fallback token")
	}
}
