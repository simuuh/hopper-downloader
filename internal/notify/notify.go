// Package notify pushes a message to external services when a package
// finishes: a generic JSON webhook and/or an ntfy topic. It is a leaf
// package — queue.Manager decides *when* a package is finished and hands
// over an Event; this package only knows how to deliver it.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

// EventPackageFinished is the only event type sent today.
const EventPackageFinished = "package_finished"

// Status summarizes how a finished package ended.
const (
	StatusCompleted = "completed" // every item done, extraction (if any) succeeded
	StatusPartial   = "partial"   // finished, but some items failed or extraction failed
	StatusFailed    = "failed"    // no item downloaded successfully
)

// Event is the payload of one notification. It is also the exact JSON body
// the generic webhook receives.
type Event struct {
	Event     string `json:"event"`
	PackageID string `json:"package_id,omitempty"`
	Package   string `json:"package"`
	Status    string `json:"status"`
	Items     int    `json:"items"`
	Done      int    `json:"done"`
	Failed    int    `json:"failed"`
	Bytes     int64  `json:"bytes"`
	// Extract is "extracted", "extract_failed" or empty (no archive, or
	// auto-extract disabled).
	Extract string `json:"extract,omitempty"`
	// Path is the package's directory relative to the downloads root.
	Path       string    `json:"path,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
}

// Config selects the delivery targets. An empty field disables that target.
type Config struct {
	WebhookURL string
	NtfyURL    string
	NtfyToken  string
}

// ErrNotConfigured is returned by Send when no target is configured.
var ErrNotConfigured = errors.New("notify: no notification target configured")

// Notifier delivers Events. The zero value is not usable; build one with New.
type Notifier struct {
	cfg    Config
	client *http.Client
	log    *slog.Logger
	wg     sync.WaitGroup
}

// New builds a Notifier. client may be nil (a 10s-timeout default is used).
func New(cfg Config, client *http.Client, log *slog.Logger) *Notifier {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Notifier{cfg: cfg, client: client, log: log}
}

// Enabled reports whether at least one target is configured.
func (n *Notifier) Enabled() bool {
	return n != nil && (n.cfg.WebhookURL != "" || n.cfg.NtfyURL != "")
}

// PackageFinished delivers ev in the background — a slow or unreachable
// target must never hold up the queue. Failures are logged, not returned.
func (n *Notifier) PackageFinished(ev Event) {
	if !n.Enabled() {
		return
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := n.Send(ctx, ev); err != nil {
			n.log.Warn("notify: delivery failed", "package", ev.Package, "error", err)
		}
	}()
}

// Wait blocks until every background delivery started so far has finished.
// Used on shutdown (and by tests) so a last notification isn't cut off.
func (n *Notifier) Wait() {
	if n != nil {
		n.wg.Wait()
	}
}

// Send delivers ev synchronously to every configured target and returns
// the joined errors of those that failed. Used directly by the "send test
// notification" API endpoint, where the caller wants the outcome.
func (n *Notifier) Send(ctx context.Context, ev Event) error {
	if !n.Enabled() {
		return ErrNotConfigured
	}
	var errs []error
	if n.cfg.WebhookURL != "" {
		if err := n.sendWebhook(ctx, ev); err != nil {
			errs = append(errs, fmt.Errorf("webhook: %w", err))
		}
	}
	if n.cfg.NtfyURL != "" {
		if err := n.sendNtfy(ctx, ev); err != nil {
			errs = append(errs, fmt.Errorf("ntfy: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (n *Notifier) sendWebhook(ctx context.Context, ev Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "hopper")
	return n.do(req)
}

// sendNtfy publishes a plain-text message using ntfy's header-based API
// (https://docs.ntfy.sh/publish/). Non-ASCII titles are RFC 2047-encoded,
// which ntfy decodes.
func (n *Notifier) sendNtfy(ctx context.Context, ev Event) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.NtfyURL, strings.NewReader(Message(ev)))
	if err != nil {
		return err
	}
	req.Header.Set("Title", mime.BEncoding.Encode("UTF-8", Title(ev)))
	req.Header.Set("Tags", "hopper,"+ev.Status)
	if ev.Status != StatusCompleted {
		req.Header.Set("Priority", "high")
	}
	if n.cfg.NtfyToken != "" {
		req.Header.Set("Authorization", "Bearer "+n.cfg.NtfyToken)
	}
	req.Header.Set("User-Agent", "hopper")
	return n.do(req)
}

func (n *Notifier) do(req *http.Request) error {
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// Title is the short headline for ev.
func Title(ev Event) string {
	switch ev.Status {
	case StatusCompleted:
		return "Finished: " + ev.Package
	case StatusFailed:
		return "Failed: " + ev.Package
	default:
		return "Finished with errors: " + ev.Package
	}
}

// Message is the human-readable body for ev.
func Message(ev Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d/%d files downloaded", ev.Done, ev.Items)
	if ev.Failed > 0 {
		fmt.Fprintf(&b, ", %d failed", ev.Failed)
	}
	if ev.Bytes > 0 {
		fmt.Fprintf(&b, " (%s)", formatBytes(ev.Bytes))
	}
	switch ev.Extract {
	case "extracted":
		b.WriteString(", extracted")
	case "extract_failed":
		b.WriteString(", extraction failed")
	}
	if ev.Path != "" {
		b.WriteString("\n" + ev.Path)
	}
	return b.String()
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
