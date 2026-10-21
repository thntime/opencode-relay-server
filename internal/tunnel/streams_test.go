package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newTestTunnel(t *testing.T, m *Manager, subdomain string) *websocket.Conn {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.HandleWebSocket(w, r, subdomain)
	}))
	header := http.Header{}
	header.Set("X-Tunnel-Protocol", "2")
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/tunnel", header)
	if err != nil {
		srv.Close()
		t.Fatalf("failed to dial fake tunnel: %v", err)
	}
	if got := resp.Header.Get("X-Tunnel-Protocol"); got != "2" {
		t.Fatalf("relay announced protocol %q, want 2", got)
	}
	t.Cleanup(func() {
		conn.Close()
		srv.Close()
	})

	deadline := time.Now().Add(3 * time.Second)
	for !m.IsConnected(subdomain) {
		if time.Now().After(deadline) {
			t.Fatalf("tunnel for %s did not connect", subdomain)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return conn
}

func writeTunnelFrame(t *testing.T, conn *websocket.Conn, frame any) {
	t.Helper()
	data, err := encodeFrame(frame)
	if err != nil {
		t.Fatalf("encodeFrame() error = %v", err)
	}
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("failed to write frame: %v", err)
	}
}

func readTunnelRequest(t *testing.T, conn *websocket.Conn) *TunnelRequest {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn.SetReadDeadline(deadline)
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("failed to read tunnel request: %v", err)
		}
		var probe struct {
			Type   string `json:"type"`
			Method string `json:"method"`
		}
		if json.Unmarshal(msg, &probe) != nil {
			continue
		}
		if probe.Type != "" || probe.Method == "" {
			continue
		}
		var req TunnelRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			t.Fatalf("failed to decode tunnel request: %v", err)
		}
		return &req
	}
}

func waitTunnelFrame(t *testing.T, conn *websocket.Conn, typ string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		conn.SetReadDeadline(deadline)
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for %q frame: %v", typ, err)
		}
		var probe map[string]any
		if json.Unmarshal(msg, &probe) != nil {
			continue
		}
		if probe["type"] == typ {
			return probe
		}
	}
}

func waitStreamCount(t *testing.T, m *Manager, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if got := m.StreamCount(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("StreamCount() = %d, want %d", m.StreamCount(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestForwardStreamUnknownHost(t *testing.T) {
	m := NewManager()
	_, err := m.ForwardStream(context.Background(), "does-not-exist", &TunnelRequest{ID: "1"})
	if !errors.Is(err, ErrTunnelNotFound) {
		t.Fatalf("ForwardStream() error = %v, want ErrTunnelNotFound", err)
	}
}

func TestForwardStreamDeliversHeadAndChunksInOrder(t *testing.T) {
	m := NewManager()
	conn := newTestTunnel(t, m, "host1")

	stream, err := m.ForwardStream(context.Background(), "host1", &TunnelRequest{ID: "s1", Method: "GET", Path: "/event"})
	if err != nil {
		t.Fatalf("ForwardStream() error = %v", err)
	}

	req := readTunnelRequest(t, conn)
	if req.ID != "s1" || req.Path != "/event" {
		t.Fatalf("forwarded request = %#v, want ID=s1 path=/event", req)
	}

	writeTunnelFrame(t, conn, &ResHead{
		ID:         req.ID,
		StatusCode: 200,
		Headers:    map[string]string{"Content-Type": "text/event-stream"},
	})
	select {
	case head := <-stream.Head:
		if head.StatusCode != 200 {
			t.Fatalf("head status = %d, want 200", head.StatusCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream head was not delivered")
	}

	want := []string{"one", "two", "three"}
	for i, part := range want {
		writeTunnelFrame(t, conn, &ResChunk{ID: req.ID, Seq: int64(i), Data: []byte(part)})
	}
	writeTunnelFrame(t, conn, &ResEnd{ID: req.ID})

	var got []string
	timeout := time.After(3 * time.Second)
	for {
		select {
		case chunk, ok := <-stream.Data:
			if !ok {
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("chunks = %v, want %v", got, want)
				}
				waitStreamCount(t, m, 0)
				return
			}
			got = append(got, string(chunk))
		case <-timeout:
			t.Fatalf("stream did not end; chunks = %v", got)
		}
	}
}

func TestForwardStreamIgnoresUnknownID(t *testing.T) {
	m := NewManager()
	conn := newTestTunnel(t, m, "host1")

	stream, err := m.ForwardStream(context.Background(), "host1", &TunnelRequest{ID: "known", Method: "GET", Path: "/event"})
	if err != nil {
		t.Fatalf("ForwardStream() error = %v", err)
	}
	readTunnelRequest(t, conn)
	writeTunnelFrame(t, conn, &ResHead{ID: "known", StatusCode: 200})
	<-stream.Head

	writeTunnelFrame(t, conn, &ResChunk{ID: "unknown", Seq: 0, Data: []byte("bad")})
	writeTunnelFrame(t, conn, &ResChunk{ID: "known", Seq: 0, Data: []byte("good")})
	writeTunnelFrame(t, conn, &ResEnd{ID: "known"})

	select {
	case chunk := <-stream.Data:
		if string(chunk) != "good" {
			t.Fatalf("chunk = %q, want good", chunk)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("valid chunk was not delivered")
	}
	if _, ok := <-stream.Data; ok {
		t.Fatal("stream should be closed after res-end")
	}
}

func TestUnregisterStreamIsIdempotent(t *testing.T) {
	m := NewManager()
	s := newStream("s1", nil, m)

	m.streamsMu.Lock()
	m.streams["s1"] = s
	m.streamsMu.Unlock()

	m.unregisterStream("s1")
	m.unregisterStream("s1")

	if _, ok := <-s.Data; ok {
		t.Fatal("stream data channel should be closed")
	}
	m.unregisterStream("never-registered")
	if got := m.StreamCount(); got != 0 {
		t.Fatalf("StreamCount() = %d, want 0", got)
	}
}

func TestStreamCancelSendsOneReqCancel(t *testing.T) {
	m := NewManager()
	conn := newTestTunnel(t, m, "host1")

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := m.ForwardStream(ctx, "host1", &TunnelRequest{ID: "cancel-1", Method: "GET", Path: "/event"})
	if err != nil {
		t.Fatalf("ForwardStream() error = %v", err)
	}
	readTunnelRequest(t, conn)

	cancel()
	cancel()
	stream.cancel()

	frame := waitTunnelFrame(t, conn, FrameReqCancel, 3*time.Second)
	if frame["id"] != "cancel-1" {
		t.Fatalf("req-cancel id = %v, want cancel-1", frame["id"])
	}

	// No second req-cancel should arrive.
	conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var probe map[string]any
		if json.Unmarshal(msg, &probe) != nil {
			continue
		}
		if probe["type"] == FrameReqCancel {
			t.Fatal("received a second req-cancel frame")
		}
	}

	waitStreamCount(t, m, 0)
}

func TestStreamBackpressureAbortsStream(t *testing.T) {
	m := NewManager()
	conn := newTestTunnel(t, m, "host1")

	stream, err := m.ForwardStream(context.Background(), "host1", &TunnelRequest{ID: "slow", Method: "GET", Path: "/event"})
	if err != nil {
		t.Fatalf("ForwardStream() error = %v", err)
	}
	readTunnelRequest(t, conn)
	writeTunnelFrame(t, conn, &ResHead{ID: "slow", StatusCode: 200})
	<-stream.Head

	// Never consume Data: the buffer must fill and abort the stream.
	for i := 0; i <= streamBuffer; i++ {
		writeTunnelFrame(t, conn, &ResChunk{ID: "slow", Seq: int64(i), Data: []byte("x")})
	}

	frame := waitTunnelFrame(t, conn, FrameReqCancel, 3*time.Second)
	if frame["id"] != "slow" {
		t.Fatalf("req-cancel id = %v, want slow", frame["id"])
	}
	waitStreamCount(t, m, 0)
	if err := stream.ErrValue(); !errors.Is(err, ErrStreamBackpressure) {
		t.Fatalf("stream error = %v, want ErrStreamBackpressure", err)
	}
}

// A close frame must never be lost to backpressure: when the ws-frame buffer
// is full the close still has to reach the mobile side.
func TestDeliverWsCloseOverridesFullFrameBuffer(t *testing.T) {
	m := NewManager()
	s := newStream("ws-close-full", nil, m)

	m.streamsMu.Lock()
	m.streams[s.ID] = s
	m.streamsMu.Unlock()

	for i := 0; i < wsFrameBuffer; i++ {
		s.Frames <- &WsFrame{ID: s.ID}
	}

	m.deliverWsClose(&WsClose{ID: s.ID, Code: websocket.CloseNormalClosure, Reason: "done"})

	select {
	case f := <-s.Close:
		if f.Code != websocket.CloseNormalClosure || f.Reason != "done" {
			t.Fatalf("close frame = %+v, want code 1000 reason done", f)
		}
	default:
		t.Fatal("deliverWsClose dropped the close frame while the data buffer was full")
	}
}

func TestStreamSlowConsumerAbortsStream(t *testing.T) {
	m := NewManager()
	m.slowConsumerTimeout = 50 * time.Millisecond

	conn := newTestTunnel(t, m, "host1")

	stream, err := m.ForwardStream(context.Background(), "host1", &TunnelRequest{ID: "stalled", Method: "GET", Path: "/event"})
	if err != nil {
		t.Fatalf("ForwardStream() error = %v", err)
	}
	readTunnelRequest(t, conn)
	writeTunnelFrame(t, conn, &ResHead{ID: "stalled", StatusCode: 200})
	<-stream.Head
	writeTunnelFrame(t, conn, &ResChunk{ID: "stalled", Seq: 0, Data: []byte("x")})

	waitTunnelFrame(t, conn, FrameReqCancel, 3*time.Second)
	waitStreamCount(t, m, 0)
	if err := stream.ErrValue(); !errors.Is(err, ErrSlowConsumer) {
		t.Fatalf("stream error = %v, want ErrSlowConsumer", err)
	}
}
