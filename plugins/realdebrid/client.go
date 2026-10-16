package realdebrid

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// max429Retries bounds retries on HTTP 429 beyond the proactive rate
// limiter below — normally the limiter alone keeps requests under
// Real-Debrid's cap, but a shared token (another process using the same
// account) or clock/timing edge effects can still trigger one.
const max429Retries = 3

// do issues one Real-Debrid API request, applying the plugin's rate
// limiter before every attempt (including retries) and retrying with a
// short fixed backoff on HTTP 429, matching the retry behavior of
// Real-Debrid's own production Go client (rclone's realdebrid backend).
// The caller is responsible for closing the returned response body.
func (p *Plugin) do(ctx context.Context, method, path, token string, form url.Values) (*http.Response, error) {
	base := p.baseURL
	if base == "" {
		base = apiBase
	}

	var resp *http.Response
	for attempt := 0; attempt <= max429Retries; attempt++ {
		if err := p.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("%s: rate limiter: %w", pluginName, err)
		}

		req, err := newRequest(ctx, base, method, path, token, form)
		if err != nil {
			return nil, err
		}

		resp, err = p.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%s: request failed: %w", pluginName, err)
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}

		_ = resp.Body.Close()
		if attempt < max429Retries {
			delay := time.Duration(attempt+1) * p.retryBaseDelay
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	// Exhausted retries; resp is the last (429) response, still valid to
	// return so the caller's statusErr(resp.StatusCode) reports it as
	// plugins.ErrRateLimited.
	return resp, nil
}

func newRequest(ctx context.Context, base, method, path, token string, form url.Values) (*http.Request, error) {
	reqURL := base + path
	var body io.Reader
	if method == http.MethodGet {
		if len(form) > 0 {
			reqURL += "?" + form.Encode()
		}
	} else if len(form) > 0 {
		body = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", pluginName, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}
