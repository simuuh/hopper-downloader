package api

import (
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsWriteTimeout = 10 * time.Second
	wsPingInterval = 30 * time.Second
	wsReadLimit    = 4096
)

// handleWebSocket upgrades to a WebSocket and streams queue.Event values
// (progress, speed, state changes) as JSON until the client disconnects
// or the server shuts down. It never reads application messages from the
// client; a background reader only exists to notice disconnects.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Warn("api: websocket upgrade failed", "error", err)
		return
	}
	defer func() { _ = conn.Close() }()

	// Clients only send control frames; anything bigger is not ours.
	conn.SetReadLimit(wsReadLimit)

	// The token was checked once at the handshake; end the session when it
	// expires instead of streaming events to it indefinitely. The client
	// reconnects with its (refreshed) token.
	var expired <-chan time.Time
	if c, ok := r.Context().Value(claimsCtxKey).(*claims); ok && c.ExpiresAt != nil {
		timer := time.NewTimer(time.Until(c.ExpiresAt.Time))
		defer timer.Stop()
		expired = timer.C
	}

	events, cancel := s.queue.Subscribe()
	defer cancel()

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if err := conn.WriteJSON(ev); err != nil {
				return
			}
		case <-ping.C:
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-closed:
			return
		case <-expired:
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "token expired"), time.Now().Add(wsWriteTimeout))
			return
		case <-r.Context().Done():
			return
		}
	}
}
