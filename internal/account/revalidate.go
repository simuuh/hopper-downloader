package account

import (
	"context"

	"github.com/simuuh/hopper-downloader/plugins"
)

// RevalidateAll re-checks every enabled account against its hoster's
// plugins.AccountValidator (if the plugin has one) and caches the result,
// so the expiry date / traffic left shown in the Web UI — and its
// "expires soon, extend premium" hint — stay current without the user
// pressing "Re-check". A hoster-call error leaves the previous cached
// result untouched (a transient outage shouldn't flip an account to
// "unknown"). Runs sequentially: a handful of accounts, a few requests.
func (m *Manager) RevalidateAll(ctx context.Context, hosters []plugins.Hoster) {
	validators := map[string]plugins.AccountValidator{}
	for _, h := range hosters {
		if v, ok := h.(plugins.AccountValidator); ok {
			validators[h.Name()] = v
		}
	}
	for _, acc := range m.List() {
		if ctx.Err() != nil {
			return
		}
		v, ok := validators[acc.HosterKey]
		if !ok || !acc.Enabled {
			continue
		}
		creds, err := m.Credentials(acc.ID)
		if err != nil {
			continue
		}
		info, err := v.ValidateAccount(creds)
		if err != nil {
			m.log.Warn("account: periodic revalidation failed", "account_id", acc.ID, "hoster_key", acc.HosterKey, "error", err)
			continue
		}
		if _, err := m.SetValidation(acc.ID, info); err != nil {
			m.log.Warn("account: store revalidation result", "account_id", acc.ID, "error", err)
		}
	}
}
