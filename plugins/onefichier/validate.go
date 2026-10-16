package onefichier

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/simuuh/hopper-downloader/plugins"
)

// premiumExpiryFields are tried, in order, against /user/info.cgi's
// response for a premium-expiration date. Unlike the fields Resolve/
// FetchDownloadURL use, this one isn't independently confirmed against
// a documented example (see this package's doc comment) — community API
// wrappers document the *request* parameters for user/info.cgi but not
// its response schema. If none of these match, ValidateAccount still
// correctly reports Valid: true from the call succeeding at all (a
// working API key is the part that matters most); ExpiresAt/Label just
// stay generic instead of showing a real date. Confirmed field names
// should replace this list once available.
var premiumExpiryFields = []string{"premium_end", "premium_expire", "expire_premium", "end_premium"}

// oneFichierDateLayout matches /file/info.cgi's "date" field format
// ("2019-01-02 08:37:23") — assumed consistent across this API rather
// than independently confirmed for the premium-expiry field specifically.
const oneFichierDateLayout = "2006-01-02 15:04:05"

// ValidateAccount implements plugins.AccountValidator via POST
// /user/info.cgi — any account-scoped endpoint would do to confirm the
// key works; this one also happens to be where premium status/expiry
// would live, if 1fichier reports it (see premiumExpiryFields).
func (p *Plugin) ValidateAccount(acc *plugins.Account) (*plugins.AccountInfo, error) {
	token := p.tokenFrom(acc)
	if token == "" {
		return nil, plugins.ErrAccountRequired
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, err := p.do(ctx, "/user/info.cgi", token, map[string]any{})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &plugins.AccountInfo{Valid: false, Label: "Invalid API key"}, nil
	}
	if err := statusErr(resp.StatusCode); err != nil {
		return nil, err
	}

	var raw map[string]json.RawMessage
	if err := decodeJSON(resp.Body, &raw); err != nil {
		return nil, fmt.Errorf("%s: decode user/info response: %w", pluginName, err)
	}
	if status, ok := raw["status"]; ok {
		var s string
		if err := json.Unmarshal(status, &s); err == nil && s == "KO" {
			return &plugins.AccountInfo{Valid: false, Label: "Invalid API key"}, nil
		}
	}

	info := &plugins.AccountInfo{Valid: true, Label: "Premium"}
	for _, field := range premiumExpiryFields {
		v, ok := raw[field]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil || s == "" {
			continue
		}
		if t, err := time.Parse(oneFichierDateLayout, s); err == nil {
			info.ExpiresAt = &t
			info.Label = "Premium until " + t.Format("2006-01-02")
			break
		}
	}

	return info, nil
}
