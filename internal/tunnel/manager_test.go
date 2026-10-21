package tunnel_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zero469/opencode-relay-server/internal/tunnel"
)

func TestManager_ReplacesConnectionForSameSubdomain(t *testing.T) {
	manager := tunnel.NewManager()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		manager.HandleWebSocket(w, r, "host1")
	}))
	t.Cleanup(srv.Close)

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/tunnel"

	conn1, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("failed to dial first connection: %v", err)
	}
	t.Cleanup(func() { conn1.Close() })
	waitConnected(t, manager, "host1")

	conn2, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("failed to dial second connection: %v", err)
	}
	t.Cleanup(func() { conn2.Close() })

	conn1.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := conn1.ReadMessage(); err == nil {
		t.Fatal("old connection was not closed after replacement")
	}

	if !manager.IsConnected("host1") {
		t.Fatal("IsConnected = false after replacement, want true")
	}

	conn2.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := conn2.WriteMessage(websocket.TextMessage, []byte(`{"type":"heartbeat_ack"}`)); err != nil {
		t.Fatalf("new connection is not usable: %v", err)
	}
}

func TestManager_ForwardRequestUnknownHost(t *testing.T) {
	manager := tunnel.NewManager()

	_, err := manager.ForwardRequest("does-not-exist", &tunnel.TunnelRequest{ID: "1"})
	if !errors.Is(err, tunnel.ErrTunnelNotFound) {
		t.Fatalf("ForwardRequest() error = %v, want ErrTunnelNotFound", err)
	}
}

func waitConnected(t *testing.T, manager *tunnel.Manager, subdomain string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !manager.IsConnected(subdomain) {
		if time.Now().After(deadline) {
			t.Fatalf("tunnel for %s did not connect", subdomain)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
