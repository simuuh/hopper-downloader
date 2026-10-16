package onefichier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// do issues one 1fichier API request: POST, JSON body, bearer auth — the
// convention every endpoint in this package uses. The caller is
// responsible for closing the returned response body.
func (p *Plugin) do(ctx context.Context, path, token string, body any) (*http.Response, error) {
	base := p.baseURL
	if base == "" {
		base = apiBase
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s: marshal request body: %w", pluginName, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", pluginName, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: request failed: %w", pluginName, err)
	}
	return resp, nil
}
