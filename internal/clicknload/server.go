// Package clicknload implements a JDownloader-compatible Click'n'Load 2
// listener: the plain /flash/add and AES-encrypted /flash/addcrypted2
// endpoints hoster websites' "Click'n'Load" buttons POST to, plus the
// /flash/ and /jdcheck.js probes those buttons use to detect that a
// compatible download tool is running locally.
//
// This listener is security-sensitive by design: any web page the user's
// browser opens can reach it and queue links. It is bound to 127.0.0.1 by
// default and only accepts requests from Config.TrustedNets (see
// internal/config's clicknload.trusted_cidrs), and everything it queues
// starts paused, so a download never begins without the user starting it.
package clicknload

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/simuuh/hopper-downloader/internal/extract"
	"github.com/simuuh/hopper-downloader/internal/queue"
)

// maxBodyBytes caps request bodies; CNL payloads (a handful of URLs) are
// always small text.
const maxBodyBytes = 1 << 20 // 1 MiB

// Sink is the subset of *queue.Manager the Click'n'Load listener needs.
// Depending on the interface (not *queue.Manager) keeps this package unit
// testable without a real queue/downloader/database stack.
type Sink interface {
	AddLinks(links []queue.LinkInput, name string, opts queue.AddOptions) (*queue.AddResult, error)
}

// Config configures the Click'n'Load listener.
type Config struct {
	Host        string
	Port        int
	TrustedNets []*net.IPNet
}

// Server serves the Click'n'Load 2 protocol on its own listener, separate
// from the main REST API/Web UI.
type Server struct {
	sink Sink
	cfg  Config
	log  *slog.Logger
	srv  *http.Server
}

// New builds a Server. Call ListenAndServe to start it.
func New(sink Sink, cfg Config, log *slog.Logger) *Server {
	return &Server{sink: sink, cfg: cfg, log: log}
}

// Handler builds the HTTP handler, wrapped in the CORS and trusted-CIDR
// gates (in that order — see corsMiddleware). It is exported mainly for
// tests, which exercise it directly without binding a real listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /flash/", s.handleCheck)
	mux.HandleFunc("GET /jdcheck.js", s.handleCheckJS)
	mux.HandleFunc("POST /flash/add", s.handleAdd)
	mux.HandleFunc("POST /flash/addcrypted2", s.handleAddCrypted2)
	return corsMiddleware(s.trustMiddleware(mux))
}

// corsMiddleware adds permissive CORS headers to every response —
// including trustMiddleware's own 403 for an untrusted client, which is
// why this wraps *outside* it rather than the other way around — and
// answers a preflight OPTIONS request unconditionally, before the trust
// check runs at all. Needed for the same reason as hopperctl's identical
// proxy middleware: a hoster site's Click'n'Load button (e.g. hide.cx,
// served over HTTPS) POSTs directly from its own page's origin to this
// listener, which is cross-origin as far as the browser's CORS
// enforcement is concerned regardless of how this listener is bound.
//
// Wildcarding the *response-readability* grant here doesn't broaden who
// can successfully queue a link: a form POST is a CORS simple request
// that reaches the handler without any preflight anyway, which is why
// addLinks always queues paused; the trust check still gates every real
// GET/POST by client IP. The header only affects whether the calling
// page's own JS can read the plain "success"/error text back afterward,
// which is what the CnL protocol's button script actually needs to do
// to report status to the user. An OPTIONS
// preflight is let through unconditionally (ahead of the trust check)
// because it carries no request body/CnL payload and triggers no queue
// action by itself — only the real POST that follows it does, and that
// still goes through trustMiddleware exactly as before.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, *")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ListenAndServe starts the listener and blocks until it stops. A clean
// Shutdown returns nil rather than http.ErrServerClosed.
func (s *Server) ListenAndServe() error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second, // CnL bodies are tiny (1 MiB cap)
		IdleTimeout:       2 * time.Minute,
	}
	s.log.Info("clicknload: listening", "addr", addr)
	err := s.srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops the listener.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

func (s *Server) trustMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip != nil {
			// An IPv4 client reaching a dual-stack listener can arrive as
			// an IPv4-mapped IPv6 address (::ffff:192.168.1.5) rather than
			// its plain IPv4 form — net.IPNet.Contains on an IPv4 CIDR
			// (the common case in trusted_cidrs, e.g. 127.0.0.1/32) does
			// not match that form even though it's the same address, so
			// unwrap it first. To4 is a no-op for a genuine IPv6 address.
			if ip4 := ip.To4(); ip4 != nil {
				ip = ip4
			}
		}
		if ip == nil || !s.isTrusted(ip) {
			s.log.Warn("clicknload: rejected untrusted client", "remote_addr", r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) isTrusted(ip net.IP) bool {
	for _, n := range s.cfg.TrustedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// handleCheck serves the probe hoster-site CNL buttons use to detect a
// compatible tool: they look for the literal substring "JDownloader" in
// the response body. Hopper still identifies itself, since the two
// requirements aren't mutually exclusive.
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, "Hopper (JDownloader-compatible Click'n'Load endpoint)")
}

func (s *Server) handleCheckJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	fmt.Fprint(w, "jdownloader=true; var version='1.0'; // served by Hopper\n")
}

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	urls := splitList(r.FormValue("urls"))
	passwords := splitList(r.FormValue("passwords"))
	s.addLinks(w, urls, passwords, r.FormValue("source"), r.FormValue("package"))
}

func (s *Server) handleAddCrypted2(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	source := r.FormValue("source")

	hexKey, err := extractHexKey(r.FormValue("jk"))
	if err != nil {
		s.log.Warn("clicknload: unsupported jk payload", "error", err, "source", source)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	plaintext, err := decryptCNL2(r.FormValue("crypted"), hexKey)
	if err != nil {
		s.log.Warn(fmt.Sprintf("clicknload: AES decrypt failed, dropping request from %s", source), "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	urls := splitList(string(plaintext))
	passwords := splitList(r.FormValue("passwords"))
	s.addLinks(w, urls, passwords, source, r.FormValue("package"))
}

// validLinkURL reports whether raw is a well-formed http(s) URL —
// exactly what queue.Manager itself would go on to reject, checked here
// too so a single bad URL in a multi-URL submission can be logged and
// skipped individually instead of being silently dropped (or silently
// failing the whole submission) deep inside AddPackage.
func validLinkURL(raw string) error {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	return nil
}

func (s *Server) addLinks(w http.ResponseWriter, urls, passwords []string, source, pkgField string) {
	if len(urls) == 0 {
		http.Error(w, "no urls", http.StatusBadRequest)
		return
	}

	valid := make([]string, 0, len(urls))
	for _, u := range urls {
		if err := validLinkURL(u); err != nil {
			s.log.Warn("clicknload: skipping invalid url", "url", u, "reason", err, "source", source)
			continue
		}
		valid = append(valid, u)
	}
	s.log.Info(fmt.Sprintf("clicknload: accepted %d/%d links", len(valid), len(urls)), "source", source)
	if len(valid) == 0 {
		http.Error(w, "no valid urls", http.StatusBadRequest)
		return
	}

	password := ""
	if len(passwords) > 0 {
		password = passwords[0]
	}
	// Always paused, regardless of auto_start_imports and of the request's
	// Origin: the trust check only sees the client IP, which for a local
	// browser is trusted no matter which web page sent the request — any
	// site can POST here without a CORS preflight. Starting a download
	// needs an explicit action in the Web UI.
	opts := queue.AddOptions{Password: password, StartPaused: true}

	inputs := make([]queue.LinkInput, len(valid))
	for i, u := range valid {
		inputs[i] = queue.LinkInput{URL: u}
	}
	// AddLinks groups the links per release, merges further parts of a
	// release that's already queued into its package and skips duplicates
	// — a second click on the same Click'n'Load button is harmless.
	res, err := s.sink.AddLinks(inputs, cnlPackageName(pkgField, source, valid), opts)
	if err != nil {
		s.log.Error("clicknload: failed to queue links", "error", err)
		http.Error(w, "failed to queue links", http.StatusInternalServerError)
		return
	}

	s.log.Info("clicknload: queued links", "count", len(res.Items), "duplicates", len(res.Duplicates), "packages", len(res.Packages), "source", source)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, "success")
}

// cnlPackageName picks the package name handed to queue.AddLinks. An
// explicit, non-generic "package" form field (sent by some CnL buttons)
// wins. For archive parts the name comes from the files themselves —
// the shared release name if there is exactly one, or "" to let AddLinks
// name one package per release; a referring site's hostname (e.g.
// "filecrypt.cc") is never a useful name for a release. Plain
// (non-archive) links fall back to packageName as before; a single plain
// link gets no package at all.
func cnlPackageName(pkgField, source string, urls []string) string {
	if p := strings.TrimSpace(pkgField); !genericPackageNames[p] {
		return p
	}
	bases := map[string]string{} // lowercased -> as spelled
	for _, u := range urls {
		if fn := filenameFromURL(u); fn != "" && extract.IsArchive(fn) {
			b := extract.GroupBase(fn)
			bases[strings.ToLower(b)] = b
		}
	}
	switch {
	case len(bases) == 1:
		for _, b := range bases {
			return b
		}
		return ""
	case len(bases) > 1:
		return ""
	case len(urls) == 1:
		return ""
	default:
		return packageName(source, urls)
	}
}

// genericPackageNames are known placeholder package names that carry no
// real information — a name candidate matching one of these is treated
// the same as an empty one (see packageName), not kept as-is the way a
// genuinely meaningful name (e.g. a referring hoster site's hostname) is.
var genericPackageNames = map[string]bool{
	"Click'n'Load": true,
	"ClickNLoad":   true,
	"click'n'load": true,
	"":             true,
}

// packageName picks a name for a multi-URL Click'n'Load submission.
// source (the hoster page's own URL, sent as CNL's "source" field) is
// tried first — its hostname is a meaningful, user-facing name (e.g.
// "links.example.org") and is kept unchanged if it is one. But CNL doesn't
// always send a useful source: it can be empty, or (some redirector
// sites) one of a handful of generic placeholder strings instead of a
// real hostname — either way, a name derived from the URLs' own
// filenames beats a folder literally named "Click'n'Load".
func packageName(source string, urls []string) string {
	if source != "" {
		if u, err := url.Parse(source); err == nil && u.Host != "" && !genericPackageNames[u.Host] {
			return u.Host
		}
	}
	return packageNameFromURLs(urls)
}

// packageNameFromURLs derives a package name from the URLs themselves:
// each one's filename (its last path segment, minus a common hoster
// "info page" suffix — see filenameFromURL) run through
// extract.GroupBase, which already knows how to strip a multi-part
// archive's part-number-and-extension suffix (e.g. "movie.part1.rar" ->
// "movie") for exactly this purpose elsewhere (queue.Manager's own
// package naming for an expanded hidecx container does the same thing).
// If every URL reduces to the same base name — the common case for a
// multi-part release, where every part shares one base — that shared
// name wins outright; otherwise the longest prefix shared across the
// raw filenames is used if it's long enough to be meaningful, falling
// back to just the first URL's own name.
func packageNameFromURLs(urls []string) string {
	names := make([]string, 0, len(urls))
	for _, u := range urls {
		if fn := filenameFromURL(u); fn != "" {
			names = append(names, fn)
		}
	}
	if len(names) == 0 {
		return "Click'n'Load"
	}

	bases := make([]string, len(names))
	for i, n := range names {
		bases[i] = extract.GroupBase(n)
	}
	if allEqual(bases) {
		return bases[0]
	}
	if prefix := commonPrefix(names); len(prefix) >= 3 {
		return prefix
	}
	return bases[0]
}

// filenameFromURL extracts a display filename from a raw hoster URL's
// last path segment: percent-decoded, and with a trailing ".html"
// stripped (many hosters serve a file's info/download page at
// .../<id>/<filename>.html, e.g. rapidgator.net — the real filename and
// its extension are still underneath that suffix).
func filenameFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	base := path.Base(u.Path)
	if base == "." || base == "/" {
		return ""
	}
	if unescaped, err := url.QueryUnescape(base); err == nil {
		base = unescaped
	}
	return strings.TrimSuffix(base, ".html")
}

func allEqual(strs []string) bool {
	if len(strs) == 0 {
		return true
	}
	for _, s := range strs[1:] {
		if s != strs[0] {
			return false
		}
	}
	return true
}

// commonPrefix returns the longest string every element of strs starts
// with, trimmed back to the last "." / "-" / "_" / space boundary so it
// doesn't end mid-token (e.g. "movie.part" rather than "movie.part1" /
// "movie.part10" cut off wherever their digits first differ).
func commonPrefix(strs []string) string {
	if len(strs) == 0 {
		return ""
	}
	prefix := strs[0]
	for _, s := range strs[1:] {
		i := 0
		for i < len(prefix) && i < len(s) && prefix[i] == s[i] {
			i++
		}
		prefix = prefix[:i]
		if prefix == "" {
			return ""
		}
	}
	if i := strings.LastIndexAny(prefix, "._- "); i > 0 {
		prefix = prefix[:i]
	}
	return prefix
}
