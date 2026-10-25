package api

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/simuuh/hopper-downloader/internal/dlc"
	"github.com/simuuh/hopper-downloader/internal/queue"
)

// maxImportBytes caps an uploaded .dlc/.txt file. Link containers are
// text; multi-megabyte "link lists" would be unusual.
const maxImportBytes = 32 << 20 // 32 MiB

// maxImportFiles caps how many files one import request may carry.
const maxImportFiles = 50

type importResponse struct {
	Packages   []*queue.Package `json:"packages"`
	Items      []*queue.Item    `json:"items"`
	Duplicates []string         `json:"duplicates"`
	// Errors reports per-file problems (unparseable .dlc, no links) in a
	// multi-file import; the request as a whole still succeeds as long as
	// at least one file produced something.
	Errors []string `json:"errors,omitempty"`
}

// handleImport accepts a multipart upload of one or more files (field
// "file", repeatable): .dlc containers (decrypted via internal/dlc,
// requiring the JDownloader key service) or plaintext link lists. Every
// container package goes through queue.AddLinks, so its links are grouped
// per release, merged into a package that already holds other parts of
// the same release (several DLCs for one release, or a DLC after a
// Click'n'Load of the first parts) and de-duplicated.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes)
	if err := r.ParseMultipartForm(maxImportBytes); err != nil { //nolint:gosec // G120: body is already capped by MaxBytesReader above
		writeError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}
	var headers []*multipart.FileHeader
	if r.MultipartForm != nil {
		headers = r.MultipartForm.File["file"]
	}
	if len(headers) == 0 {
		writeError(w, http.StatusBadRequest, "file is required")
		return
	}
	if len(headers) > maxImportFiles {
		writeError(w, http.StatusBadRequest, "too many files in one import")
		return
	}

	set := s.settings.Get()
	prio, err := queue.ParsePriority(set.DefaultPriority)
	if err != nil {
		prio = queue.PriorityNormal
	}
	opts := queue.AddOptions{StartPaused: !set.AutoStartImports, Priority: prio}
	resp := importResponse{Duplicates: []string{}}
	seenPkg := map[string]bool{}
	for _, header := range headers {
		container, err := s.parseImportFile(r, header)
		if err != nil {
			resp.Errors = append(resp.Errors, header.Filename+": "+err.Error())
			continue
		}
		added := 0
		for _, pkg := range container.Packages {
			res, err := s.queue.AddLinks(linkInputs(pkg), importPackageName(pkg, header.Filename), withPassword(opts, pkg.Password))
			if err != nil {
				s.log.Warn("api: import failed to queue package", "file", header.Filename, "package", pkg.Name, "error", err)
				continue
			}
			for _, p := range res.Packages {
				if !seenPkg[p.ID] {
					seenPkg[p.ID] = true
					resp.Packages = append(resp.Packages, p)
				}
			}
			resp.Items = append(resp.Items, res.Items...)
			resp.Duplicates = append(resp.Duplicates, res.Duplicates...)
			added += len(res.Items) + len(res.Duplicates)
		}
		if added == 0 {
			resp.Errors = append(resp.Errors, header.Filename+": no valid links found")
		}
	}

	if len(resp.Items) == 0 && len(resp.Duplicates) == 0 {
		msg := "no valid links found in uploaded file"
		if len(resp.Errors) > 0 {
			msg = strings.Join(resp.Errors, "; ")
		}
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// parseImportFile reads and parses one uploaded file.
func (s *Server) parseImportFile(r *http.Request, header *multipart.FileHeader) (*dlc.Container, error) {
	file, err := header.Open()
	if err != nil {
		return nil, errors.New("failed to read uploaded file")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxImportBytes))
	if err != nil {
		return nil, errors.New("failed to read uploaded file")
	}

	if !strings.HasSuffix(strings.ToLower(header.Filename), ".dlc") {
		return dlc.ParsePlaintext(data), nil
	}
	if !s.cfg.DLCEnabled {
		return nil, errors.New("DLC import is disabled (dlc.enabled: false)")
	}
	container, err := dlc.Parse(r.Context(), s.dlcClient, s.cfg.DLCAPIURL, data)
	if err != nil {
		// Details only in the log: the error can carry the key service's
		// URL with the container's key in it.
		s.log.Warn("api: dlc parse failed", "filename", header.Filename, "error", err)
		return nil, errors.New("failed to parse .dlc file (see hopperd log for details)")
	}
	return container, nil
}

// importPackageName is the name handed to AddLinks for one container
// package: the DLC's own package name if it has one (used for a
// single-release package, see queue.AddLinks), else "" so the release
// name is derived from the files. A plaintext list's synthetic "Imported
// links" name is treated as no name, the uploaded file name only as a
// last resort for non-archive links.
func importPackageName(pkg dlc.Package, filename string) string {
	name := strings.TrimSpace(pkg.Name)
	if name != "" && name != "Imported links" {
		return name
	}
	if len(pkg.Files) <= 1 {
		return ""
	}
	for _, f := range pkg.Files {
		if f.Filename != "" {
			return ""
		}
	}
	return strings.TrimSuffix(filename, filepath.Ext(filename))
}

func withPassword(opts queue.AddOptions, password string) queue.AddOptions {
	opts.Password = password
	return opts
}

// linkInputs converts a container package's files into queue.LinkInputs,
// keeping each file's real name and size — known up front for a .dlc,
// unlike a plain URL. Passing the name is what lets sequential_parts
// ordering and auto-extract grouping recognize a multi-part RAR set
// before any download starts.
func linkInputs(pkg dlc.Package) []queue.LinkInput {
	out := make([]queue.LinkInput, 0, len(pkg.Files))
	for _, f := range pkg.Files {
		if f.URL == "" {
			continue
		}
		out = append(out, queue.LinkInput{URL: f.URL, Filename: f.Filename, Size: f.Size})
	}
	return out
}

type addLinksRequest struct {
	// Text is free text containing one or more http(s) URLs (one per
	// line, or pasted from anywhere — see dlc.ExtractURLs).
	Text     string   `json:"text"`
	URLs     []string `json:"urls"`
	Name     string   `json:"name"`
	Password string   `json:"password"`
	Priority string   `json:"priority"`
	Path     string   `json:"path"`
}

// handleAddLinks (POST /api/v1/links/bulk) queues every URL found in
// Text/URLs through queue.AddLinks — grouped per release, merged into
// existing packages of the same release, duplicates skipped — and
// optionally applies a "save to" path to each resulting package.
func (s *Server) handleAddLinks(w http.ResponseWriter, r *http.Request) {
	var req addLinksRequest
	if err := decodeJSON(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	urls := append(dlc.ExtractURLs(req.Text), req.URLs...)
	if len(urls) == 0 {
		writeError(w, http.StatusBadRequest, "no http(s) URLs found")
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

	inputs := make([]queue.LinkInput, len(urls))
	for i, u := range urls {
		inputs[i] = queue.LinkInput{URL: u}
	}
	name := strings.TrimSpace(req.Name)
	path := strings.TrimSpace(req.Path)
	if path != "" && name == "" && len(urls) == 1 {
		// A "save to" path needs a package to live on; a lone plain link
		// would otherwise be added package-less.
		name = path
	}
	res, err := s.queue.AddLinks(inputs, name, queue.AddOptions{Priority: priority, Password: req.Password})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if path != "" {
		for i, p := range res.Packages {
			if updated, err := s.queue.SetPackagePath(p.ID, path); err == nil {
				res.Packages[i] = updated
			}
		}
	}
	writeJSON(w, http.StatusCreated, res)
}
