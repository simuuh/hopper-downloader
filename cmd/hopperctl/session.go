package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// session is hopperctl's local, persisted login state: which hopperd
// server to talk to and the JWT obtained from it. It is stored with
// user-only permissions since the token grants full API access.
type session struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

// apiTimeout bounds every request to hopperd's API. http.DefaultClient
// has none, so an unreachable or hanging server left hopperctl waiting
// forever.
const apiTimeout = 60 * time.Second

func sessionPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hopper", "hopperctl.json"), nil
}

func loadSession() (*session, error) {
	p, err := sessionPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("no saved session (run `hopperctl login` first): %w", err)
	}
	var s session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse session file %s: %w", p, err)
	}
	return &s, nil
}

func saveSession(s *session) error {
	p, err := sessionPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

// readStdinTrimmed reads a password piped into stdin. It strips a
// leading UTF-8 BOM (Windows PowerShell prepends one to piped string
// literals, e.g. `"pw" | hopperctl login`) and trailing CR/LF, so the
// same command works the same way across shells.
func readStdinTrimmed() (string, error) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	return strings.TrimRight(string(data), "\r\n"), nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// apiRequestUnauthenticated issues a request without a bearer token, for
// the login call itself (before a session exists).
func apiRequestUnauthenticated(method, url string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := (&http.Client{Timeout: apiTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("request to %s failed: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// apiRequest issues an authenticated request against the saved session's
// server and decodes a JSON response body into out (if non-nil). A
// non-2xx status is returned as an error including the response body.
func apiRequest(method, path string, body any, out any) error {
	sess, err := loadSession()
	if err != nil {
		return err
	}

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, strings.TrimRight(sess.Server, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+sess.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := (&http.Client{Timeout: apiTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("request to %s failed: %w", sess.Server, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
