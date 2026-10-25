package downloader

import (
	"errors"
	"net/url"
	"strings"
)

// redactURL shortens a direct download link to scheme, host and file
// name for error messages. Debrid and hoster direct links work like
// bearer tokens — the token sits in the query or in a path segment
// (/d/<token>/name) — and errors end up in the item's stored error, the
// Web UI and the logs.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<url>"
	}
	name := ""
	if i := strings.LastIndex(u.Path, "/"); i >= 0 && i < len(u.Path)-1 {
		name = u.Path[i+1:]
	}
	if name == "" {
		return u.Scheme + "://" + u.Host + "/…"
	}
	return u.Scheme + "://" + u.Host + "/…/" + name
}

// redactErr rewrites the URL of every *url.Error in err's chain (what
// net/http returns for a failed request, redirect targets included) with
// redactURL. It must run on the error straight from the client, before
// anything wraps it: fmt.Errorf renders the message when it is called.
func redactErr(err error) error {
	for e := err; e != nil; {
		var ue *url.Error
		if !errors.As(e, &ue) {
			break
		}
		ue.URL = redactURL(ue.URL)
		e = ue.Err
	}
	return err
}
