package tunnel_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zero469/opencode-relay-server/internal/tunnel"
)

type fakeStreamTunnel struct {
	conn   *websocket.Conn
	reqs   chan *tunnel.TunnelRequest
	frames chan map[string]any
}

func connectStreamTunnel(t *testing.T, manager *tunnel.Manager, subdomain string) *fakeStreamTunnel {
	t.Helper()

	conn := connectFakeTunnel(t, manager, subdomain, nil, "2")
	ft := &fakeStreamTunnel{
		conn:   conn,
		reqs:   make(chan *tunnel.TunnelRequest, 64),
		frames: make(chan map[string]any, 256),
	}

	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var obj map[string]any
			if json.Unmarshal(msg, &obj) != nil {
				continue
			}
			if typ, _ := obj["type"].(string); typ != "" {
				select {
				case ft.frames <- obj:
				default:
				}
				continue
			}
			if method, _ := obj["method"].(string); method != "" {
				var req tunnel.TunnelRequest
				if json.Unmarshal(msg, &req) == nil {
					select {
					case ft.reqs <- &req:
					default:
					}
				}
			}
		}
	}()

	return ft
}

func (ft *fakeStreamTunnel) write(t *testing.T, v any) {
	t.Helper()
	ft.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := ft.conn.WriteJSON(v); err != nil {
		t.Fatalf("failed to write tunnel frame: %v", err)
	}
}

func (ft *fakeStreamTunnel) nextRequest(t *testing.T) *tunnel.TunnelRequest {
	t.Helper()
	select {
	case req := <-ft.reqs:
		return req
	case <-time.After(3 * time.Second):
		t.Fatal("request was not forwarded to the tunnel")
		return nil
	}
}

func (ft *fakeStreamTunnel) waitFrame(t *testing.T, typ string) map[string]any {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case frame := <-ft.frames:
			if frame["type"] == typ {
				return frame
			}
		case <-deadline:
			t.Fatalf("tunnel frame %q was not received", typ)
			return nil
		}
	}
}

func streamHead(id string, headers map[string]string) map[string]any {
	return map[string]any{
		"type":        "res-head",
		"id":          id,
		"status_code": http.StatusOK,
		"headers":     headers,
	}
}

func streamChunk(id string, seq int, data string) map[string]any {
	return map[string]any{
		"type": "res-chunk",
		"id":   id,
		"seq":  seq,
		"data": base64.StdEncoding.EncodeToString([]byte(data)),
	}
}

type sseResult struct {
	resp *http.Response
	err  error
}

// startSSE issues the mobile-side SSE request asynchronously: the relay only
// responds after res-head arrives, so the test must send frames first.
func startSSE(srv *httptest.Server, token, path string, ctx context.Context) <-chan sseResult {
	ch := make(chan sseResult, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		if err != nil {
			ch <- sseResult{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "text/event-stream")

		resp, err := srv.Client().Do(req)
		ch <- sseResult{resp: resp, err: err}
	}()
	return ch
}

func (r sseResult) must(t *testing.T) *http.Response {
	t.Helper()
	if r.err != nil {
		t.Fatalf("SSE request failed: %v", r.err)
	}
	return r.resp
}

func waitStreamCount(t *testing.T, manager *tunnel.Manager, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if manager.StreamCount() == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("StreamCount() = %d, want %d", manager.StreamCount(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRelayStreaming_HeadFlushedBeforeChunks(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	ft := connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resCh := startSSE(srv, env.token1, "/relay/"+env.device1.Subdomain+"/event", ctx)

	req := ft.nextRequest(t)
	if req.Path != "/event" {
		t.Fatalf("forwarded path = %q, want /event", req.Path)
	}
	if req.Method != http.MethodGet {
		t.Fatalf("forwarded method = %q, want GET", req.Method)
	}

	ft.write(t, streamHead(req.ID, map[string]string{
		"Content-Type": "text/event-stream",
		"X-Test":       "yes",
	}))

	// The response headers only arrive once the relay flushed res-head. No
	// chunk has been produced yet at this point.
	resp := (<-resCh).must(t)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := resp.Header.Get("X-Test"); got != "yes" {
		t.Fatalf("X-Test = %q, want yes", got)
	}

	parts := []string{"data: one\n\n", "data: two\n\n", "data: three\n\n"}
	for i, part := range parts {
		ft.write(t, streamChunk(req.ID, i, part))
	}
	ft.write(t, map[string]any{"type": "res-end", "id": req.ID})

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read stream body: %v", err)
	}
	want := parts[0] + parts[1] + parts[2]
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
	waitStreamCount(t, env.manager, 0)
}

func TestRelayStreaming_ErrorBeforeHeadIs502(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	ft := connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resCh := startSSE(srv, env.token1, "/relay/"+env.device1.Subdomain+"/event", ctx)

	req := ft.nextRequest(t)
	ft.write(t, map[string]any{"type": "res-error", "id": req.ID, "message": "dial failed"})

	resp := (<-resCh).must(t)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	waitStreamCount(t, env.manager, 0)
}

func TestRelayStreaming_ErrorAfterHeadTruncatesStream(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	ft := connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resCh := startSSE(srv, env.token1, "/relay/"+env.device1.Subdomain+"/event", ctx)

	req := ft.nextRequest(t)
	ft.write(t, streamHead(req.ID, map[string]string{"Content-Type": "text/event-stream"}))

	resp := (<-resCh).must(t)
	defer resp.Body.Close()

	ft.write(t, streamChunk(req.ID, 0, "partial"))
	ft.write(t, map[string]any{"type": "res-error", "id": req.ID, "message": "upstream failed"})

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read stream body: %v", err)
	}
	if string(body) != "partial" {
		t.Fatalf("body = %q, want partial", body)
	}
	waitStreamCount(t, env.manager, 0)
}

func TestRelayStreaming_ClientDisconnectSendsReqCancel(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	ft := connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	ctx, cancel := context.WithCancel(context.Background())
	resCh := startSSE(srv, env.token1, "/relay/"+env.device1.Subdomain+"/event", ctx)

	req := ft.nextRequest(t)
	ft.write(t, streamHead(req.ID, map[string]string{"Content-Type": "text/event-stream"}))

	resp := (<-resCh).must(t)
	cancel()
	resp.Body.Close()

	frame := ft.waitFrame(t, "req-cancel")
	if frame["id"] != req.ID {
		t.Fatalf("req-cancel id = %v, want %s", frame["id"], req.ID)
	}
	waitStreamCount(t, env.manager, 0)
}

func TestRelayStreaming_OversizedBodyIs413(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	body := bytes.Repeat([]byte("a"), (1<<20)+1)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/relay/"+env.device1.Subdomain+"/event", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+env.token1)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestRelayStreaming_TooManyStreamsIs429(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	const limit = 16
	cancels := make([]context.CancelFunc, 0, limit)
	for i := 0; i < limit; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		if _, err := env.manager.ForwardStream(ctx, env.device1.Subdomain, &tunnel.TunnelRequest{
			ID:     fmt.Sprintf("held-%d", i),
			Method: http.MethodGet,
			Path:   "/held",
		}); err != nil {
			t.Fatalf("ForwardStream(%d) error = %v", i, err)
		}
	}
	if got := env.manager.StreamCount(); got != limit {
		t.Fatalf("StreamCount() = %d, want %d", got, limit)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resCh := startSSE(srv, env.token1, "/relay/"+env.device1.Subdomain+"/event", ctx)
	resp := (<-resCh).must(t)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}

	for _, cancelHeld := range cancels {
		cancelHeld()
	}
	waitStreamCount(t, env.manager, 0)
}
