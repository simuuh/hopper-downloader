package downloader

import (
	"errors"
	"net/http"
)

// maxRedirects matches net/http's own default limit.
const maxRedirects = 10

// errRedirectDowngrade is returned for a redirect from https to plain
// http. Download URLs often carry a session token in the query string;
// following such a redirect would send the follow-up request, and with
// it whatever the next hop puts into its URL, unencrypted.
var errRedirectDowngrade = errors.New("refusing redirect from https to http")

// checkRedirect is the engine client's redirect policy: at most
// maxRedirects hops and never from https down to http. The error text
// holds no URL, so no token can reach logs through it.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errRedirectDowngrade
	}
	return nil
}
