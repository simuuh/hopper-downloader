package api

import (
	"net/http"

	"github.com/simuuh/hopper-downloader/internal/queue"
	"github.com/simuuh/hopper-downloader/plugins"
)

// pluginInfo describes one hoster plugin the Web UI's "add account" form
// can offer. Key is the canonical identifier used everywhere else
// (account hoster_key, hopper.yaml per-hoster config) — plugin.Name().
type pluginInfo struct {
	Name    string   `json:"name"`
	Key     string   `json:"key"`
	Domains []string `json:"domains"`
	// PremiumURL links to the hoster's page for buying/extending premium
	// (see plugins.PremiumLinker); IsReferral marks it as the developer's
	// referral link so the UI can disclose that.
	PremiumURL string `json:"premium_url,omitempty"`
	IsReferral bool   `json:"is_referral,omitempty"`
	// Fields describes the account form (see plugins.CredentialDescriber).
	Fields []plugins.CredentialField `json:"fields"`
	// Rank is the plugin's place in the account priority (1 = tried
	// first when several plugins with an account can handle a link).
	Rank int `json:"rank"`
}

// credentialedPlugins returns every registered plugin that needs an
// account: plugins.Credentialed returning true. A plugin that doesn't
// implement plugins.Credentialed at all (e.g. directhttp, which has no
// account concept) is excluded, same as one that implements it and
// returns false.
func (s *Server) credentialedPlugins() []plugins.Hoster {
	var out []plugins.Hoster
	for _, h := range s.plugins.All() {
		cred, ok := h.(plugins.Credentialed)
		if !ok || !cred.Credentialed() {
			continue
		}
		out = append(out, h)
	}
	return out
}

// hosterByKey returns the registered plugin whose Name() equals key.
func (s *Server) hosterByKey(key string) (plugins.Hoster, bool) {
	for _, h := range s.plugins.All() {
		if h.Name() == key {
			return h, true
		}
	}
	return nil, false
}

// isCredentialedHosterKey reports whether hosterKey matches the Name() of
// a registered plugin that requires an account. Used to validate
// addAccountRequest.HosterKey server-side, so account creation can't bind
// to a plugin that doesn't take credentials (e.g. directhttp) or to a
// key that doesn't correspond to any registered plugin at all.
func (s *Server) isCredentialedHosterKey(hosterKey string) bool {
	for _, h := range s.credentialedPlugins() {
		if h.Name() == hosterKey {
			return true
		}
	}
	return false
}

// handleListPlugins returns every credentialed plugin in account
// priority order (see queue.RankHosters and the account_priority
// setting), for the Web UI's "add account" form and priority list.
func (s *Server) handleListPlugins(w http.ResponseWriter, r *http.Request) {
	credentialed := queue.RankHosters(s.credentialedPlugins(), s.settings.Get().AccountPriority)
	out := make([]pluginInfo, 0, len(credentialed))
	for i, h := range credentialed {
		name := h.Name()
		if dn, ok := h.(plugins.DisplayNamer); ok {
			name = dn.DisplayName()
		}
		link, isRef := plugins.PremiumLink(h, s.cfg.ReferralLinks)
		out = append(out, pluginInfo{Name: name, Key: h.Name(), Domains: h.Domains(), PremiumURL: link, IsReferral: isRef, Fields: plugins.CredentialFieldsFor(h), Rank: i + 1})
	}
	writeJSON(w, http.StatusOK, out)
}
