package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

// defaultProxyAddr is where the CnL proxy listens locally. Hoster
// "Click'n'Load" buttons are hardcoded to probe 127.0.0.1:9666, so this
// must match regardless of what port the real hopperd's CnL listener
// uses on the target/NAS side.
const defaultProxyAddr = "127.0.0.1:9666"

// proxyRequestTimeout bounds a single forwarded request to the target
// hopperd. CnL payloads are a handful of URLs; there is no reason a
// forward should ever take long, and an unreachable/hung target must not
// wedge the local listener.
const proxyRequestTimeout = 10 * time.Second

// maxProxyBodyBytes caps request bodies the proxy will read, matching
// internal/clicknload's own cap — CnL payloads are always small text.
const maxProxyBodyBytes = 1 << 20 // 1 MiB

// proxyServer answers the two CnL probe endpoints locally (a hoster site
// only needs to see *something* listening on 127.0.0.1:9666) and forwards
// the two POST endpoints, which actually queue links, to the real hopperd
// CnL listener configured as Config.Target.
type proxyServer struct {
	target string
	client *http.Client
	log    *slog.Logger
}

func newProxyServer(target string, log *slog.Logger) *proxyServer {
	return &proxyServer{
		target: target,
		client: &http.Client{Timeout: proxyRequestTimeout},
		log:    log,
	}
}

func (p *proxyServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /flash/", p.handleCheck)
	mux.HandleFunc("GET /jdcheck.js", p.handleCheckJS)
	mux.HandleFunc("POST /flash/add", p.handleForward("/flash/add"))
	mux.HandleFunc("POST /flash/addcrypted2", p.handleForward("/flash/addcrypted2"))
	return corsMiddleware(mux)
}

// corsMiddleware adds permissive CORS headers to every response and
// answers a preflight OPTIONS request directly, before it ever reaches
// the mux. Needed because a hoster site's Click'n'Load button (e.g.
// hide.cx, served over HTTPS) POSTs directly from its own page's origin
// to this http://127.0.0.1:9666 origin — that's cross-origin as far as
// the browser's CORS enforcement is concerned, entirely independent of
// this server only accepting connections on 127.0.0.1 in the first
// place (CORS governs what a browser lets calling-page JS *read back*
// from a response; it has nothing to do with which sockets can be
// reached). Wildcarding it is fine specifically because of that
// loopback-only binding (see defaultProxyAddr/-addr): nothing but a
// browser already running on this same machine can reach it at all, so
// there's no meaningful "which origins" restriction to enforce past
// that. (internal/clicknload's own listener needs the identical fix for
// the same reason, for whoever runs hopperd locally without hopperctl
// in front of it — see its own corsMiddleware and doc comment for why
// that one additionally runs after the trusted-CIDR gate, unlike here.)
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

func (p *proxyServer) handleCheck(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, "JDownloader - Hopper")
}

func (p *proxyServer) handleCheckJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	fmt.Fprint(w, "jdownloader=true; var version='2.0';\n")
}

// handleForward relays a POST body to path on the target hopperd's CnL
// listener as-is (form-encoded, so the Content-Type header must survive
// the hop) and mirrors its status code and body back. A target that
// can't be reached at all — down NAS, wrong address, network partition —
// is logged as a warning and answered 503, never a crash: this proxy is
// meant to run unattended (as a service), so one bad NAS reboot can't
// take it down.
func (p *proxyServer) handleForward(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p.target == "" {
			p.log.Warn("hopperctl: proxy has no target configured", "path", path)
			http.Error(w, "hopperctl: no target configured (see hopperctl.yaml)", http.StatusServiceUnavailable)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxProxyBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, joinURL(p.target, path), noopReadCloserFrom(body))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "" {
			req.Header.Set("Content-Type", ct)
		}

		resp, err := p.client.Do(req)
		if err != nil {
			p.log.Warn("hopperctl: proxy target unreachable", "path", path, "target", p.target, "error", err)
			http.Error(w, "hopperd unreachable", http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		p.log.Info("hopperctl: proxy forwarded request", "path", path, "target", p.target, "status", resp.StatusCode)
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

// sniffPeekTimeout bounds how long dualProtocolListener waits for a new
// connection's first byte before giving up on it — a jdcheck.js <script>
// tag's plain HTTP GET and an HTTPS fetch's TLS ClientHello both send
// their first byte immediately once connected, so this only guards
// against a connection that opens and then never sends anything.
const sniffPeekTimeout = 5 * time.Second

// tlsRecordTypeHandshake is the first byte of every TLS record carrying a
// handshake message (RFC 8446 §5.1) — in particular a ClientHello, which
// is always the very first byte a TLS client sends. Anything else is
// treated as plaintext HTTP.
const tlsRecordTypeHandshake = 0x16

// dualProtocolListener wraps a net.Listener so a single port can serve
// both plain HTTP and HTTPS. Real hoster Click'n'Load integrations
// address 127.0.0.1:9666 with either scheme depending on how the hoster
// page embeds the request: a `<script src="http://127.0.0.1:9666/jdcheck.js">`
// tag is not scheme-upgraded by the browser the way an HTTPS page's own
// fetch/XHR POST is (see runProxy's doc comment for why HTTPS is still
// required for that side) — some hoster pages (e.g. filecrypt.cc) load
// jdcheck.js this way. A TLS-only listener doesn't just refuse that
// request, it actively breaks it: Go's net/http answers a plaintext
// request on a TLS listener with a literal "400 Bad Request: Client sent
// an HTTP request to an HTTPS server", raw over the same TCP connection,
// before any handler ever runs — confirmed live against filecrypt.cc's
// jdcheck.js probe. Fix: peek the first byte of every accepted
// connection and route it to either the plain HTTP path or a
// tls.Server-wrapped one, on the same port — the same technique
// connection multiplexers like cmux use.
type dualProtocolListener struct {
	net.Listener
	tlsConfig *tls.Config
}

// Accept swallows a per-connection peek failure (idle/dead connection,
// peek timeout) by closing that connection and trying the next one,
// rather than returning the error to the caller — http.Server.Serve
// treats any error Accept returns as fatal to the whole listener, so
// propagating a single bad connection's error here would take down the
// entire proxy. A real Listener-level failure (e.g. the listener was
// closed for shutdown) still returns immediately, unchanged.
func (l *dualProtocolListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}

		_ = conn.SetReadDeadline(time.Now().Add(sniffPeekTimeout))
		br := bufio.NewReader(conn)
		first, err := br.Peek(1)
		_ = conn.SetReadDeadline(time.Time{})
		if err != nil {
			_ = conn.Close()
			continue
		}

		pc := &peekedConn{Conn: conn, r: br}
		if first[0] == tlsRecordTypeHandshake {
			return tls.Server(pc, l.tlsConfig), nil
		}
		return pc, nil
	}
}

// peekedConn is a net.Conn whose Read is satisfied from a bufio.Reader
// that already buffered the connection's first byte(s) via Peek — needed
// so dualProtocolListener can inspect a connection's first byte without
// consuming it out from under whatever reads the conn next (the raw
// http.Server for plaintext, or tls.Server for a handshake).
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func joinURL(base, path string) string {
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	return base + path
}

// noopReadCloserFrom adapts an in-memory body into an io.ReadCloser
// http.NewRequestWithContext accepts, without a second copy of the bytes.
func noopReadCloserFrom(b []byte) io.ReadCloser {
	return io.NopCloser(&byteReader{b: b})
}

type byteReader struct{ b []byte }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

// runProxy blocks serving the CnL proxy over both HTTP and HTTPS, on the
// same port, until ctx is cancelled, then shuts down gracefully. Shared
// by `hopperctl proxy` (standalone) and `hopperctl run` (alongside the
// watcher).
//
// Both protocols are genuinely required, for two different parts of the
// same CnL handshake:
//   - HTTPS: an HTTPS hoster page's Click'n'Load button POSTs directly
//     from its own page origin to this listener via fetch/XHR, and a
//     browser refuses that as mixed content against a plain
//     http://127.0.0.1:9666 target regardless of CORS or loopback-only
//     binding — see ensureCert's doc comment.
//   - HTTP: some hoster pages (e.g. filecrypt.cc) load the jdcheck.js
//     detection probe via a plain `<script src="http://...">` tag rather
//     than fetch/XHR — browsers don't scheme-upgrade a script tag's src
//     the way they block/upgrade an active fetch, so this arrives as a
//     genuine plaintext HTTP request. Serving HTTPS-only for it doesn't
//     just refuse that request, it breaks it outright: Go's net/http
//     answers a plaintext request on a TLS listener with a literal
//     "400 Bad Request: Client sent an HTTP request to an HTTPS server"
//     — confirmed live.
//
// dualProtocolListener resolves this by sniffing each connection's first
// byte and routing it to the right path on the same port, rather than
// needing two separate ports (real Click'n'Load buttons are hardcoded to
// 127.0.0.1:9666, so a second port isn't an option anyway). certDir is
// where the self-signed certificate this proxy presents is persisted
// (see certDir()/ensureCert) — passed in rather than resolved here so
// tests can point it at a throwaway directory instead of the real one
// under %APPDATA%/~/.config.
func runProxy(ctx context.Context, addr, target, certDir string, log *slog.Logger) error {
	cert, err := ensureCert(certDir)
	if err != nil {
		return fmt.Errorf("hopperctl: prepare TLS certificate: %w", err)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("hopperctl: listen on %s: %w", addr, err)
	}
	dl := &dualProtocolListener{
		Listener:  ln,
		tlsConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}

	srv := &http.Server{
		Handler:           newProxyServer(target, log).handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second, // CnL bodies are tiny (1 MiB cap)
		IdleTimeout:       2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("hopperctl: proxy listening (HTTP+HTTPS)", "addr", addr, "target", target)
		log.Info("hopperctl: ACTION REQUIRED on first use",
			"msg", "open https://127.0.0.1:9666/flash/ in each browser and accept the certificate warning; "+
				"this is required once per browser profile before Click'n'Load buttons that POST over HTTPS will work (see docs/REMOTE_CLICKNLOAD.md)")
		err := srv.Serve(dl)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return <-errCh
	case err := <-errCh:
		return err
	}
}

func cmdProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	addr := fs.String("addr", defaultProxyAddr, "local address to listen on")
	target := fs.String("target", "", "hopperd CnL endpoint (overrides hopperctl.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *target != "" {
		cfg.Target = *target
	}
	if cfg.Target == "" {
		return fmt.Errorf("no target configured: set `target:` in %s or pass -target", cfg.Path)
	}

	dir, err := certDir()
	if err != nil {
		return fmt.Errorf("resolve TLS certificate directory: %w", err)
	}

	log := newLogger(cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runProxy(ctx, *addr, cfg.Target, dir, log)
}
