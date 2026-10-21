package tunnel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A WebSocket stream that never gets its ws-ack must be cancelled, not just
// unregistered locally: otherwise the host keeps its local socket and the
// pumpLocalWs goroutine alive forever.
func TestForwardWebSocketAckTimeoutCancelsStream(t *testing.T) {
	oldTimeout := wsAckTimeout
	wsAckTimeout = 50 * time.Millisecond
	t.Cleanup(func() { wsAckTimeout = oldTimeout })

	m := NewManager()
	host := newTestTunnel(t, m, "host1")

	h := &RelayHandler{manager: m}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.forwardWebSocket(w, r, "host1", "/pty")
	}))
	t.Cleanup(srv.Close)

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("failed to dial relay websocket: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	open := waitTunnelFrame(t, host, FrameWsOpen, 3*time.Second)
	if id, _ := open["id"].(string); id == "" {
		t.Fatalf("ws-open id missing: %v", open)
	}

	// Deliberately never acknowledge: the relay must tell the host to stop.
	frame := waitTunnelFrame(t, host, FrameReqCancel, 3*time.Second)
	if frame["id"] != open["id"] {
		t.Fatalf("req-cancel id = %v, want %v", frame["id"], open["id"])
	}

	waitStreamCount(t, m, 0)
}
