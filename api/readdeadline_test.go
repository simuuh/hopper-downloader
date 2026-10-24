package api

import (
	"bufio"
	"fmt"
	"net"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/simuuh/hopper-downloader/internal/queue"
)

// A client trickling a request body must not hold the connection open
// forever, not even on the unauthenticated login endpoint (M7).
func TestReadDeadline_SlowBodyIsCut(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	srv.bodyTimeout = 200 * time.Millisecond
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(httpSrv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// Announces 100 bytes, sends one.
	if _, err := fmt.Fprint(conn, "POST /api/v1/auth/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{"); err != nil {
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err = bufio.NewReader(conn).ReadString('\n')
	for err == nil {
		_, err = bufio.NewReader(conn).ReadString('\n')
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("connection still open after %s, want the server to end it", time.Since(start).Round(time.Millisecond))
	}
}

// The WebSocket outlives the per-request deadline of its handshake.
func TestReadDeadline_WebSocketStaysOpen(t *testing.T) {
	srv, fq := newTestServer(t, nil)
	srv.bodyTimeout = 100 * time.Millisecond
	token := loginAndGetToken(t, srv)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws?" + url.Values{"token": {token}}.Encode()
	conn, dialResp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if dialResp != nil {
		defer dialResp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	time.Sleep(400 * time.Millisecond) // well past the deadline
	fq.publish(queue.Event{Type: queue.EventItemUpdated, Item: &queue.Item{ID: "item-1"}})

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var ev queue.Event
	if err := conn.ReadJSON(&ev); err != nil {
		t.Fatalf("read after the request deadline: %v", err)
	}
}
