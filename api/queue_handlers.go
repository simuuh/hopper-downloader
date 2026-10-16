package api

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/simuuh/hopper-downloader/internal/dlc"
	"github.com/simuuh/hopper-downloader/internal/queue"
)

type addLinkRequest struct {
	URL         string     `json:"url"`
	Priority    string     `json:"priority"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	Filename    string     `json:"filename"`
	Password    string     `json:"password"`
	HosterKey   string     `json:"hoster_key"`
}

// handleAddLink (and handleAddPackage below) accept an arbitrary
// caller-supplied URL with no SSRF allowlist/denylist — no block on
// loopback, RFC1918/link-local ranges, or cloud metadata endpoints
// before it's handed to a Hoster plugin's Resolve/FetchDownloadURL (see
// plugins/directhttp's matching doc comment) and ultimately fetched by
// downloader.Engine. This is intentional: every request reaching this
// handler already passed s.auth's JWT middleware, and Hopper's threat
// model treats an authenticated caller as trusted to name any URL — the
// same trust boundary curl/wget operate under — not as a potentially
// hostile input needing network-level restriction. Restricting it would
// also break legitimate self-hosted/homelab targets (an internal
// NAS/Nextcloud URL is a normal thing to queue here for this audience).
func (s *Server) handleAddLink(w http.ResponseWriter, r *http.Request) {
	var req addLinkRequest
	if err := decodeJSON(w, r, &req, 16*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	if req.Priority == "" {
		req.Priority = s.settings.Get().DefaultPriority
	}
	priority, err := queue.ParsePriority(req.Priority)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	item, err := s.queue.AddLink(req.URL, queue.AddOptions{
		Priority:    priority,
		ScheduledAt: req.ScheduledAt,
		Filename:    req.Filename,
		Password:    req.Password,
		HosterKey:   req.HosterKey,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

type addPackageRequest struct {
	Name        string     `json:"name"`
	URLs        []string   `json:"urls"`
	Priority    string     `json:"priority"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	Password    string     `json:"password"`
}

type addPackageResponse struct {
	Package *queue.Package `json:"package"`
	Items   []*queue.Item  `json:"items"`
}

func (s *Server) handleAddPackage(w http.ResponseWriter, r *http.Request) {
	var req addPackageRequest
	if err := decodeJSON(w, r, &req, 256*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" || len(req.URLs) == 0 {
		writeError(w, http.StatusBadRequest, "name and urls are required")
		return
	}
	if req.Priority == "" {
		req.Priority = s.settings.Get().DefaultPriority
	}
	priority, err := queue.ParsePriority(req.Priority)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	pkg, items, err := s.queue.AddPackage(req.Name, req.URLs, queue.AddOptions{
		Priority:    priority,
		ScheduledAt: req.ScheduledAt,
		Password:    req.Password,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, addPackageResponse{Package: pkg, Items: items})
}

func (s *Server) handleListQueue(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.queue.List())
}

func (s *Server) handleListPackages(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.queue.ListPackages())
}

func (s *Server) handleGetQueueItem(w http.ResponseWriter, r *http.Request) {
	item, ok := s.queue.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.queue.Pause(id); err != nil {
		writeQueueActionError(w, err)
		return
	}
	item, _ := s.queue.Get(id)
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.queue.Resume(id); err != nil {
		writeQueueActionError(w, err)
		return
	}
	item, _ := s.queue.Get(id)
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.queue.Retry(id); err != nil {
		writeQueueActionError(w, err)
		return
	}
	item, _ := s.queue.Get(id)
	writeJSON(w, http.StatusOK, item)
}

type retryExtractRequest struct {
	// Password, if set, is tried ahead of the persisted extract_passwords
	// list for this attempt only — never written to settings. The body
	// itself is optional (a bare POST with no body re-extracts using
	// whatever's already configured).
	Password string `json:"password"`
}

// handleRetryExtract implements POST /api/v1/queue/{id}/extract: re-runs
// extraction for an item currently stuck in ExtractStateExtractFailed.
// An empty request body is valid (io.EOF from decodeJSON is treated the
// same as an absent "password" field, not a malformed request) since
// Password is the request's only field and entirely optional.
func (s *Server) handleRetryExtract(w http.ResponseWriter, r *http.Request) {
	var req retryExtractRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	id := r.PathValue("id")
	if err := s.queue.RetryExtraction(id, req.Password); err != nil {
		writeRetryExtractError(w, err)
		return
	}
	item, _ := s.queue.Get(id)
	writeJSON(w, http.StatusOK, item)
}

func writeRetryExtractError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, queue.ErrExtractNotRetryable):
		writeError(w, http.StatusBadRequest, "item is not done or does not have a failed extraction")
	case errors.Is(err, queue.ErrArchiveMissing):
		writeError(w, http.StatusBadRequest, "archive file not found on disk")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

type solveCaptchaRequest struct {
	Solution string `json:"solution"`
}

// handleSolveCaptcha implements POST /api/v1/queue/{id}/captcha: submits
// a solution for an item currently blocked on a plugins.ErrCaptchaRequired
// challenge from its plugins.FreeDownloader fallback (see
// queue.Manager.run/tryFreeFallback). An empty solution is rejected
// outright (400) rather than forwarded — there's nothing useful a plugin
// could do with it, and it would just consume the retry for no reason.
func (s *Server) handleSolveCaptcha(w http.ResponseWriter, r *http.Request) {
	var req solveCaptchaRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Solution == "" {
		writeError(w, http.StatusBadRequest, "solution is required")
		return
	}
	id := r.PathValue("id")
	if err := s.queue.SolveCaptcha(id, req.Solution); err != nil {
		writeSolveCaptchaError(w, err)
		return
	}
	item, _ := s.queue.Get(id)
	writeJSON(w, http.StatusOK, item)
}

func writeSolveCaptchaError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, queue.ErrNoCaptchaPending):
		writeError(w, http.StatusBadRequest, "item has no pending captcha challenge")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.queue.Cancel(r.PathValue("id")); err != nil {
		writeQueueActionError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type bulkCancelRequest struct {
	IDs []string `json:"ids"`
}

type bulkCancelResponse struct {
	Deleted []string `json:"deleted"`
	Failed  []string `json:"failed"`
}

// handleBulkCancel implements DELETE /api/v1/queue: cancel every id in
// the body in one request, one rate-limit hit — added because firing
// one DELETE /api/v1/queue/{id} per selected item (the Web UI's
// previous "Clear done"/bulk-cancel behavior) could trip the API's
// per-client rate limiter on a large enough batch. A per-id failure
// (e.g. an id that no longer exists) doesn't fail the whole request —
// it's reported back in failed, same "partial failure is not a whole-
// batch error" principle the Web UI's own bulk-action aggregation
// already applies on its side.
func (s *Server) handleBulkCancel(w http.ResponseWriter, r *http.Request) {
	var req bulkCancelRequest
	if err := decodeJSON(w, r, &req, 256*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required")
		return
	}
	deleted, failed := s.queue.CancelMany(req.IDs)
	writeJSON(w, http.StatusOK, bulkCancelResponse{Deleted: deleted, Failed: failed})
}

type reorderRequest struct {
	IDs []string `json:"ids"`
}

func (s *Server) handleReorder(w http.ResponseWriter, r *http.Request) {
	var req reorderRequest
	if err := decodeJSON(w, r, &req, 256*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.queue.Reorder(req.IDs); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePauseAll(w http.ResponseWriter, r *http.Request) {
	if err := s.queue.PauseAll(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResumeAll(w http.ResponseWriter, r *http.Request) {
	if err := s.queue.ResumeAll(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type patchQueueItemRequest struct {
	Priority string  `json:"priority"`
	URL      *string `json:"url"`
	Filename *string `json:"filename"`
}

// handlePatchQueueItem changes an item's priority and/or — while it is
// queued, paused or failed — its URL (e.g. a working mirror for a dead
// link) and filename. See queue.Manager.EditItem.
func (s *Server) handlePatchQueueItem(w http.ResponseWriter, r *http.Request) {
	var req patchQueueItemRequest
	if err := decodeJSON(w, r, &req, 8192); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Priority == "" && req.URL == nil && req.Filename == nil {
		writeError(w, http.StatusBadRequest, "priority, url or filename is required")
		return
	}
	id := r.PathValue("id")
	if req.URL != nil || req.Filename != nil {
		if _, err := s.queue.EditItem(id, queue.ItemEdit{URL: req.URL, Filename: req.Filename}); err != nil {
			writeEditItemError(w, err)
			return
		}
	}
	if req.Priority != "" {
		priority, err := queue.ParsePriority(req.Priority)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.queue.SetPriority(id, priority); err != nil {
			writeQueueActionError(w, err)
			return
		}
	}
	item, _ := s.queue.Get(id)
	writeJSON(w, http.StatusOK, item)
}

func writeEditItemError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, queue.ErrItemNotEditable), errors.Is(err, queue.ErrDuplicateURL):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

type addToPackageRequest struct {
	// Text is free text containing one or more http(s) URLs, like
	// POST /api/v1/links/bulk.
	Text string   `json:"text"`
	URLs []string `json:"urls"`
}

// handleAddToPackage (POST /api/v1/packages/{id}/links) adds links to an
// existing package — a missing part, a replacement mirror. Duplicates are
// skipped and reported, same as POST /api/v1/links/bulk.
func (s *Server) handleAddToPackage(w http.ResponseWriter, r *http.Request) {
	var req addToPackageRequest
	if err := decodeJSON(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	urls := append(dlc.ExtractURLs(req.Text), req.URLs...)
	if len(urls) == 0 {
		writeError(w, http.StatusBadRequest, "no http(s) URLs found")
		return
	}
	priority, err := queue.ParsePriority(s.settings.Get().DefaultPriority)
	if err != nil {
		priority = queue.PriorityNormal
	}
	inputs := make([]queue.LinkInput, len(urls))
	for i, u := range urls {
		inputs[i] = queue.LinkInput{URL: u}
	}
	res, err := s.queue.AddToPackage(r.PathValue("id"), inputs, queue.AddOptions{Priority: priority})
	if err != nil {
		if errors.Is(err, queue.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

type patchPackageRequest struct {
	Path *string `json:"path"`
	Name *string `json:"name"`
}

// handlePatchPackage changes the "save to" path override (see
// queue.Manager.SetPackagePath) and/or the display name. Both are
// pointers so an explicit `{"path": ""}` (clear the override, fall back to
// the package name) is distinguishable from an absent field.
func (s *Server) handlePatchPackage(w http.ResponseWriter, r *http.Request) {
	var req patchPackageRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Path == nil && req.Name == nil {
		writeError(w, http.StatusBadRequest, "path or name is required")
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name must not be empty")
		return
	}
	id := r.PathValue("id")
	var pkg *queue.Package
	var err error
	if req.Name != nil {
		if pkg, err = s.queue.RenamePackage(id, strings.TrimSpace(*req.Name)); err != nil {
			writeQueueActionError(w, err)
			return
		}
	}
	if req.Path != nil {
		if pkg, err = s.queue.SetPackagePath(id, *req.Path); err != nil {
			writeQueueActionError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, pkg)
}

type packageActionResponse struct {
	Changed int `json:"changed"`
}

// handlePackageAction applies pause/resume/retry to every item of a
// package (POST /api/v1/packages/{id}/{action}).
func (s *Server) handlePackageAction(w http.ResponseWriter, r *http.Request) {
	action := queue.PackageAction(r.PathValue("action"))
	switch action {
	case queue.PackageActionPause, queue.PackageActionResume, queue.PackageActionRetry:
	default:
		writeError(w, http.StatusNotFound, "unknown package action")
		return
	}
	s.applyPackageAction(w, r.PathValue("id"), action)
}

// handleDeletePackage removes a package and all its items from the queue
// (DELETE /api/v1/packages/{id}). Downloaded files on disk are kept,
// same as cancelling a single item.
func (s *Server) handleDeletePackage(w http.ResponseWriter, r *http.Request) {
	s.applyPackageAction(w, r.PathValue("id"), queue.PackageActionDelete)
}

func (s *Server) applyPackageAction(w http.ResponseWriter, id string, action queue.PackageAction) {
	n, err := s.queue.ApplyPackageAction(id, action)
	if err != nil {
		writeQueueActionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, packageActionResponse{Changed: n})
}

func writeQueueActionError(w http.ResponseWriter, err error) {
	if errors.Is(err, queue.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeError(w, http.StatusConflict, err.Error())
}

type queueActionsRequest struct {
	IDs    []string `json:"ids"`
	Action string   `json:"action"`
}

// handleQueueActions applies pause/resume/retry/delete to several items
// in one request (POST /api/v1/queue/actions) — the Web UI's
// multi-select. One request instead of N parallel ones: resume/retry are
// applied atomically so the items start top to bottom, and a big
// selection can't trip the rate limiter.
func (s *Server) handleQueueActions(w http.ResponseWriter, r *http.Request) {
	var req queueActionsRequest
	if err := decodeJSON(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required")
		return
	}
	n, err := s.queue.ApplyAction(req.IDs, queue.PackageAction(req.Action))
	if err != nil {
		if errors.Is(err, queue.ErrInvalidAction) {
			writeError(w, http.StatusBadRequest, "action must be one of pause, resume, retry, delete")
			return
		}
		writeQueueActionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, packageActionResponse{Changed: n})
}
