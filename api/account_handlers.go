package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/simuuh/hopper-downloader/internal/account"
	"github.com/simuuh/hopper-downloader/plugins"
)

type addAccountRequest struct {
	HosterKey string            `json:"hoster_key"`
	Username  string            `json:"username"`
	Password  string            `json:"password"`
	Extra     map[string]string `json:"extra"`
}

func (s *Server) handleAddAccount(w http.ResponseWriter, r *http.Request) {
	var req addAccountRequest
	if err := decodeJSON(w, r, &req, 16*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.HosterKey == "" {
		writeError(w, http.StatusBadRequest, "hoster_key is required")
		return
	}
	if !s.isCredentialedHosterKey(req.HosterKey) {
		writeError(w, http.StatusBadRequest, "hoster_key does not match a registered plugin that accepts an account")
		return
	}
	// Only the fields this hoster actually reads are required — a
	// token-only hoster (Real-Debrid, 1fichier, ...) has no username.
	hoster, _ := s.hosterByKey(req.HosterKey)
	for _, f := range plugins.CredentialFieldsFor(hoster) {
		if !f.Required {
			continue
		}
		if (f.Name == "username" && req.Username == "") || (f.Name == "password" && req.Password == "") {
			writeError(w, http.StatusBadRequest, strings.ToLower(f.Label)+" is required")
			return
		}
	}

	acc, err := s.accounts.Add(req.HosterKey, req.Username, req.Password, req.Extra)
	if err != nil {
		s.log.Error("api: add account failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to add account")
		return
	}

	// Validate inline so the caller finds out immediately whether the
	// credentials actually work, rather than only discovering it when
	// the first download fails. Best-effort: a plugin that doesn't
	// implement AccountValidator (rapidgator) or a hoster-side failure
	// (network, unexpected response) leaves Valid unset (unknown) rather
	// than blocking account creation, which already succeeded above.
	acc = s.validateInline(acc, &plugins.Account{Username: req.Username, Password: req.Password, Extra: req.Extra})
	writeJSON(w, http.StatusCreated, acc)
}

// validateInline best-effort validates creds for acc's hoster and returns
// acc with the cached result applied (unchanged on any failure).
func (s *Server) validateInline(acc *account.Account, creds *plugins.Account) *account.Account {
	hoster, ok := s.hosterByKey(acc.HosterKey)
	if !ok {
		return acc
	}
	validator, ok := hoster.(plugins.AccountValidator)
	if !ok {
		return acc
	}
	info, err := validator.ValidateAccount(creds)
	if err != nil {
		s.log.Warn("api: inline account validation failed", "hoster_key", acc.HosterKey, "error", err)
		return acc
	}
	if updated, err := s.accounts.SetValidation(acc.ID, info); err == nil {
		return updated
	}
	return acc
}

type updateAccountRequest struct {
	Username string            `json:"username"`
	Password string            `json:"password"`
	Extra    map[string]string `json:"extra"`
}

// handleUpdateAccount (PUT /api/v1/accounts/{id}) replaces an account's
// credentials in place — typically after renewing/extending premium,
// when the hoster issued a new API token — instead of deleting and
// re-adding it. Empty fields keep the stored value. Re-validates inline
// like POST /accounts.
func (s *Server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	var req updateAccountRequest
	if err := decodeJSON(w, r, &req, 16*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Username == "" && req.Password == "" && req.Extra == nil {
		writeError(w, http.StatusBadRequest, "nothing to update")
		return
	}
	id := r.PathValue("id")
	acc, err := s.accounts.UpdateCredentials(id, req.Username, req.Password, req.Extra)
	if err != nil {
		if errors.Is(err, account.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.log.Error("api: update account failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update account")
		return
	}
	if creds, err := s.accounts.Credentials(id); err == nil {
		acc = s.validateInline(acc, creds)
	}
	writeJSON(w, http.StatusOK, acc)
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.accounts.List())
}

type setEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleSetAccountEnabled(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req setEnabledRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.accounts.SetEnabled(id, req.Enabled); err != nil {
		s.writeAccountActionError(w, err)
		return
	}
	acc, _ := s.accounts.Get(id)
	writeJSON(w, http.StatusOK, acc)
}

// handleValidateAccount is the "Re-check" action: unlike the inline
// validation on add, a hoster-side failure here is reported as an error
// rather than silently leaving the cached state alone — the user
// explicitly asked for a fresh answer.
func (s *Server) handleValidateAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	acc, ok := s.accounts.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	hoster, ok := s.hosterByKey(acc.HosterKey)
	if !ok {
		writeError(w, http.StatusBadRequest, "no registered plugin for this account's hoster")
		return
	}
	validator, ok := hoster.(plugins.AccountValidator)
	if !ok {
		writeError(w, http.StatusBadRequest, "this hoster does not support account validation")
		return
	}

	creds, err := s.accounts.Credentials(id)
	if err != nil {
		s.log.Error("api: load account credentials failed", "account_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load account credentials")
		return
	}

	info, err := validator.ValidateAccount(creds)
	if err != nil {
		s.log.Warn("api: account validation failed", "account_id", id, "error", err)
		writeError(w, http.StatusBadGateway, "validation request to hoster failed: "+err.Error())
		return
	}

	updated, err := s.accounts.SetValidation(id, info)
	if err != nil {
		s.writeAccountActionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.accounts.Delete(r.PathValue("id")); err != nil {
		s.writeAccountActionError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeAccountActionError answers with a generic message for anything
// but "not found": the error text comes from the database or crypto
// layer and is for the log, not the client.
func (s *Server) writeAccountActionError(w http.ResponseWriter, err error) {
	if errors.Is(err, account.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.log.Error("api: account action failed", "error", err)
	writeError(w, http.StatusInternalServerError, "account update failed (see hopperd log)")
}
