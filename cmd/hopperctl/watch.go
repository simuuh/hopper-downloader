package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// watchPollInterval controls how often the watched directory is
// rescanned. No new dependencies are allowed for Phase 2 (see
// docs/ARCHITECTURE.md), so this polls with os.ReadDir instead of pulling in an
// fsnotify-style OS-event watcher — perfectly adequate for a directory
// that only ever receives a browser's finished .dlc/.txt downloads, not
// a high-churn filesystem.
const watchPollInterval = 1 * time.Second

// settleDelay is how long a newly-seen file is left alone before being
// read: browsers write the final bytes and rename into place, and a
// download that's still in flight (a .crdownload/.part sibling, or the
// final file not yet fully flushed) must not be read mid-write.
const settleDelay = 500 * time.Millisecond

// importTimeout bounds a single import POST to hopperd. .dlc/.txt files
// are always small text (see api/import_handlers.go's own cap), so a
// hung target must not wedge the watch loop indefinitely.
const importTimeout = 30 * time.Second

func isImportCandidate(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".dlc") || strings.HasSuffix(lower, ".txt")
}

// seedSeen records every .dlc/.txt file already present in dir without
// importing it, so a (re)started watcher never re-floods hopperd with
// files a prior run already handled.
func seedSeen(dir string) map[string]bool {
	seen := make(map[string]bool)
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && isImportCandidate(e.Name()) {
				seen[e.Name()] = true
			}
		}
	}
	return seen
}

// runWatch polls dir for new .dlc/.txt files until ctx is cancelled,
// importing each one against apiTarget (hopperd's REST API base URL —
// see Config.ResolvedAPITarget, NOT the CnL proxy target the session
// happens to have logged into) using token for auth (the endpoint is
// JWT-authenticated, unlike the CnL proxy which needs no auth beyond
// trusted_cidrs). Files already present at startup are recorded but not
// imported, so restarting the watcher never re-floods hopperd with old
// files that a prior run already handled (in particular when
// delete_after is false and they're still sitting in the directory).
func runWatch(ctx context.Context, dir string, deleteAfter bool, apiTarget, token string, log *slog.Logger) error {
	seen := seedSeen(dir)

	log.Info("hopperctl: watching directory", "dir", dir, "api_target", apiTarget, "delete_after", deleteAfter)
	client := &http.Client{Timeout: importTimeout}
	ticker := time.NewTicker(watchPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			scanOnce(dir, seen, deleteAfter, apiTarget, token, client, log)
		}
	}
}

func scanOnce(dir string, seen map[string]bool, deleteAfter bool, apiTarget, token string, client *http.Client, log *slog.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Warn("hopperctl: failed to read watch directory", "dir", dir, "error", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() || !isImportCandidate(e.Name()) || seen[e.Name()] {
			continue
		}
		seen[e.Name()] = true
		time.Sleep(settleDelay)

		path := filepath.Join(dir, e.Name())
		count, err := importFile(apiTarget, token, client, path)
		if err != nil {
			log.Warn("hopperctl: import failed", "file", e.Name(), "error", err)
			continue
		}
		log.Info("hopperctl: imported file", "file", e.Name(), "items", count)

		if deleteAfter {
			if err := os.Remove(path); err != nil {
				log.Warn("hopperctl: failed to delete imported file", "file", e.Name(), "error", err)
			}
		}
	}
}

type importResult struct {
	Items []json.RawMessage `json:"items"`
}

// importFile POSTs path as a multipart "file" field to apiTarget's
// /api/v1/import, the same endpoint the Web UI's dropzone uses,
// authenticated with token (from the saved login session).
func importFile(apiTarget, token string, client *http.Client, path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return 0, err
	}
	if _, err := part.Write(data); err != nil {
		return 0, err
	}
	if err := mw.Close(); err != nil {
		return 0, err
	}

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(apiTarget, "/")+"/api/v1/import", &buf)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request to %s failed: %w", apiTarget, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result importResult
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("decode response: %w", err)
	}
	return len(result.Items), nil
}

func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory to watch (overrides hopperctl.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *dir != "" {
		cfg.WatchDir = *dir
	}

	sess, err := loadSession()
	if err != nil {
		return fmt.Errorf("watch needs a logged-in session to call /api/v1/import: %w", err)
	}

	apiTarget := cfg.ResolvedAPITarget()
	if apiTarget == "" {
		return fmt.Errorf("no api_target (or target to derive it from) configured: set `target:`/`api_target:` in %s", cfg.Path)
	}

	log := newLogger(cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runWatch(ctx, cfg.WatchDir, cfg.DeleteAfter, apiTarget, sess.Token, log)
}
