package tunnel_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zero469/opencode-relay-server/internal/tunnel"
)

// dialRelayWebSocket opens the mobile-side WebSocket through the authenticated
// /relay/ surface.
func dialRelayWebSocket(t *testing.T, srv *httptest.Server, token, path string, subprotocols []string) *websocket.Conn {
	t.Helper()

	dialer := &websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
		Subprotocols:     subprotocols,
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)

	conn, resp, err := dialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+path, header)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("websocket dial failed (status=%d): %v", status, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// acceptWS waits for the relay's ws-open and acknowledges it.
func acceptWS(t *testing.T, ft *fakeStreamTunnel) string {
	t.Helper()

	open := ft.waitFrame(t, "ws-open")
	id, _ := open["id"].(string)
	if id == "" {
		t.Fatalf("ws-open id missing: %v", open)
	}
	ft.write(t, map[string]any{
		"type":   "ws-ack",
		"id":     id,
		"ok":     true,
		"status": http.StatusSwitchingProtocols,
	})
	return id
}

func wsFramePayload(t *testing.T, frame map[string]any) (int, []byte) {
	t.Helper()

	opcode := 0
	if op, ok := frame["opcode"].(float64); ok {
		opcode = int(op)
	}
	raw, _ := frame["data"].(string)
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("ws-frame data is not base64: %v", err)
	}
	return opcode, data
}

func expectCloseCode(t *testing.T, conn *websocket.Conn, want int) *websocket.CloseError {
	t.Helper()

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatal("expected the relay to close the websocket, read succeeded")
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("read error = %v, want *websocket.CloseError", err)
	}
	if closeErr.Code != want {
		t.Fatalf("close code = %d (%q), want %d", closeErr.Code, closeErr.Text, want)
	}
	return closeErr
}

func TestRelayWebSocket_BidirectionalText(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	ft := connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	client := dialRelayWebSocket(t, srv, env.token1, "/relay/"+env.device1.Subdomain+"/pty?session=1", nil)

	open := ft.waitFrame(t, "ws-open")
	id, _ := open["id"].(string)
	if id == "" {
		t.Fatalf("ws-open id missing: %v", open)
	}
	if open["path"] != "/pty?session=1" {
		t.Errorf("ws-open path = %v, want /pty?session=1", open["path"])
	}

	ft.write(t, map[string]any{"type": "ws-ack", "id": id, "ok": true, "status": http.StatusSwitchingProtocols})

	// mobile -> host
	if err := client.WriteMessage(websocket.TextMessage, []byte("hello host")); err != nil {
		t.Fatalf("failed to write client frame: %v", err)
	}
	frame := ft.waitFrame(t, "ws-frame")
	if frame["id"] != id {
		t.Errorf("ws-frame id = %v, want %s", frame["id"], id)
	}
	opcode, data := wsFramePayload(t, frame)
	if opcode != websocket.TextMessage {
		t.Errorf("ws-frame opcode = %d, want %d", opcode, websocket.TextMessage)
	}
	if string(data) != "hello host" {
		t.Errorf("ws-frame data = %q, want %q", data, "hello host")
	}

	// host -> mobile
	ft.write(t, map[string]any{
		"type":   "ws-frame",
		"id":     id,
		"opcode": websocket.TextMessage,
		"data":   base64.StdEncoding.EncodeToString([]byte("hello mobile")),
	})
	client.SetReadDeadline(time.Now().Add(3 * time.Second))
	messageType, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read client frame: %v", err)
	}
	if messageType != websocket.TextMessage {
		t.Errorf("message type = %d, want %d", messageType, websocket.TextMessage)
	}
	if string(payload) != "hello mobile" {
		t.Errorf("payload = %q, want %q", payload, "hello mobile")
	}

	// Binary frames keep their opcode too.
	ft.write(t, map[string]any{
		"type":   "ws-frame",
		"id":     id,
		"opcode": websocket.BinaryMessage,
		"data":   base64.StdEncoding.EncodeToString([]byte{0x00, 0x01, 0xff}),
	})
	client.SetReadDeadline(time.Now().Add(3 * time.Second))
	messageType, payload, err = client.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read binary frame: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Errorf("message type = %d, want %d", messageType, websocket.BinaryMessage)
	}
	if !bytes.Equal(payload, []byte{0x00, 0x01, 0xff}) {
		t.Errorf("payload = %v, want binary bytes", payload)
	}
}

func TestRelayWebSocket_SubprotocolPassthrough(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	ft := connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	client := dialRelayWebSocket(t, srv, env.token1, "/relay/"+env.device1.Subdomain+"/pty", []string{"terminal.v1"})
	if got := client.Subprotocol(); got != "terminal.v1" {
		t.Fatalf("negotiated subprotocol = %q, want terminal.v1", got)
	}

	open := ft.waitFrame(t, "ws-open")
	subs, _ := open["subprotocols"].([]any)
	if len(subs) != 1 || subs[0] != "terminal.v1" {
		t.Fatalf("ws-open subprotocols = %v, want [terminal.v1]", open["subprotocols"])
	}

	id, _ := open["id"].(string)
	ft.write(t, map[string]any{"type": "ws-ack", "id": id, "ok": true, "status": http.StatusSwitchingProtocols, "subprotocol": "terminal.v1"})

	// Closing normally must not surface as an error to either side.
	client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	closeFrame := ft.waitFrame(t, "ws-close")
	if code, _ := closeFrame["code"].(float64); int(code) != websocket.CloseNormalClosure {
		t.Errorf("ws-close code = %v, want %d", closeFrame["code"], websocket.CloseNormalClosure)
	}
	waitStreamCount(t, env.manager, 0)
}

// When the host negotiates a different subprotocol than the one the relay
// already sent to the mobile client, the session must not continue: the client
// would parse frames with the wrong protocol.
func TestRelayWebSocket_SubprotocolMismatchCloses1011(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	ft := connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	client := dialRelayWebSocket(t, srv, env.token1, "/relay/"+env.device1.Subdomain+"/pty", []string{"a"})
	if got := client.Subprotocol(); got != "a" {
		t.Fatalf("negotiated subprotocol = %q, want a", got)
	}

	open := ft.waitFrame(t, "ws-open")
	id, _ := open["id"].(string)
	ft.write(t, map[string]any{
		"type":        "ws-ack",
		"id":          id,
		"ok":          true,
		"status":      http.StatusSwitchingProtocols,
		"subprotocol": "b",
	})

	closeErr := expectCloseCode(t, client, websocket.CloseInternalServerErr)
	if !strings.Contains(closeErr.Text, "subprotocol") {
		t.Errorf("close reason = %q, want it to mention the subprotocol mismatch", closeErr.Text)
	}
	waitStreamCount(t, env.manager, 0)
}

func TestRelayWebSocket_AckFailureClosesClient(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	ft := connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	client := dialRelayWebSocket(t, srv, env.token1, "/relay/"+env.device1.Subdomain+"/pty", nil)

	open := ft.waitFrame(t, "ws-open")
	id, _ := open["id"].(string)
	ft.write(t, map[string]any{
		"type":   "ws-ack",
		"id":     id,
		"ok":     false,
		"status": http.StatusBadGateway,
		"error":  "dial refused",
	})

	closeErr := expectCloseCode(t, client, websocket.CloseInternalServerErr)
	if !strings.Contains(closeErr.Text, "dial refused") {
		t.Errorf("close reason = %q, want it to mention the host error", closeErr.Text)
	}
	waitStreamCount(t, env.manager, 0)
}

func TestRelayWebSocket_ClientClosePropagates(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	ft := connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	client := dialRelayWebSocket(t, srv, env.token1, "/relay/"+env.device1.Subdomain+"/pty", nil)
	acceptWS(t, ft)

	const code = 4001
	if err := client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, "bye"), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("failed to send client close: %v", err)
	}

	frame := ft.waitFrame(t, "ws-close")
	if got, _ := frame["code"].(float64); int(got) != code {
		t.Errorf("ws-close code = %v, want %d", frame["code"], code)
	}
	if frame["reason"] != "bye" {
		t.Errorf("ws-close reason = %v, want bye", frame["reason"])
	}
	waitStreamCount(t, env.manager, 0)
}

func TestRelayWebSocket_HostClosePropagates(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	ft := connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	client := dialRelayWebSocket(t, srv, env.token1, "/relay/"+env.device1.Subdomain+"/pty", nil)
	id := acceptWS(t, ft)

	ft.write(t, map[string]any{"type": "ws-close", "id": id, "code": 4002, "reason": "host done"})

	closeErr := expectCloseCode(t, client, 4002)
	if closeErr.Text != "host done" {
		t.Errorf("close reason = %q, want %q", closeErr.Text, "host done")
	}
	waitStreamCount(t, env.manager, 0)
}

func TestRelayWebSocket_HTTP2Is426(t *testing.T) {
	env := newRelayTestEnv(t)
	connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	req := httptest.NewRequest(http.MethodGet, "/relay/"+env.device1.Subdomain+"/pty", nil)
	req.Header.Set("Authorization", "Bearer "+env.token1)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Proto, req.ProtoMajor, req.ProtoMinor = "HTTP/2.0", 2, 0

	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426; body = %s", rec.Code, rec.Body.String())
	}
}

func TestRelayWebSocket_OversizedFrameClosesStream(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	ft := connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	client := dialRelayWebSocket(t, srv, env.token1, "/relay/"+env.device1.Subdomain+"/pty", nil)
	acceptWS(t, ft)

	tooLarge := bytes.Repeat([]byte("a"), tunnel.MaxFrameBytes+1)
	if err := client.WriteMessage(websocket.BinaryMessage, tooLarge); err != nil {
		t.Fatalf("failed to write oversized frame: %v", err)
	}

	frame := ft.waitFrame(t, "ws-close")
	if code, _ := frame["code"].(float64); int(code) != websocket.CloseMessageTooBig {
		t.Errorf("ws-close code = %v, want %d", frame["code"], websocket.CloseMessageTooBig)
	}
	expectCloseCode(t, client, websocket.CloseMessageTooBig)
	waitStreamCount(t, env.manager, 0)
}

func TestRelayWebSocket_DuplicateCloseAfterUnregisterIsSafe(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	ft := connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	client := dialRelayWebSocket(t, srv, env.token1, "/relay/"+env.device1.Subdomain+"/pty", nil)
	id := acceptWS(t, ft)

	client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	ft.waitFrame(t, "ws-close")
	waitStreamCount(t, env.manager, 0)

	// The stream is gone; duplicate terminal frames must be ignored instead of
	// panicking or resurrecting it.
	ft.write(t, map[string]any{"type": "ws-close", "id": id, "code": 1000, "reason": "duplicate"})
	ft.write(t, map[string]any{"type": "ws-frame", "id": id, "opcode": websocket.TextMessage, "data": base64.StdEncoding.EncodeToString([]byte("late"))})
	ft.write(t, map[string]any{"type": "ws-ack", "id": id, "ok": true})

	if got := env.manager.StreamCount(); got != 0 {
		t.Fatalf("StreamCount() = %d after duplicate close, want 0", got)
	}

	// The tunnel must still serve new sessions after the duplicate frames.
	second := dialRelayWebSocket(t, srv, env.token1, "/relay/"+env.device1.Subdomain+"/pty", nil)
	acceptWS(t, ft)
	if err := second.WriteMessage(websocket.TextMessage, []byte("still alive")); err != nil {
		t.Fatalf("second session write failed: %v", err)
	}
	frame := ft.waitFrame(t, "ws-frame")
	if _, data := wsFramePayload(t, frame); string(data) != "still alive" {
		t.Fatalf("second session data = %q, want %q", data, "still alive")
	}
}

// A tunnel that never announced protocol 2 must be refused for streaming and
// WebSocket requests instead of receiving frames it cannot decode.
func TestRelayStreaming_LegacyTunnelGets501(t *testing.T) {
	env := newRelayTestEnv(t)
	connectFakeTunnel(t, env.manager, env.device1.Subdomain, nil)

	for name, headers := range map[string]map[string]string{
		"sse": {"Accept": "text/event-stream"},
		"ws":  {"Connection": "Upgrade", "Upgrade": "websocket"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/relay/"+env.device1.Subdomain+"/pty", nil)
		req.Header.Set("Authorization", "Bearer "+env.token1)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s: status = %d, want 501; body = %s", name, rec.Code, rec.Body.String())
		}
	}
}
