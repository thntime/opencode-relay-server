package tunnel_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"math/rand"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// binaryPayload returns deterministic pseudo-random bytes that include the
// values an encoding layer is most likely to mangle.
func binaryPayload(n int) []byte {
	r := rand.New(rand.NewSource(42))
	data := make([]byte, n)
	r.Read(data)
	if n > 4 {
		data[0] = 0x00
		data[1] = 0xff
		data[2] = 0x00
		data[n-1] = 0xff
	}
	return data
}

// streamChunkBytes is streamChunk for arbitrary (non-UTF-8) payloads.
func streamChunkBytes(id string, seq int, data []byte) map[string]any {
	return map[string]any{
		"type": "res-chunk",
		"id":   id,
		"seq":  seq,
		"data": base64.StdEncoding.EncodeToString(data),
	}
}

// The relay must forward res-chunk payloads byte for byte: no trimming, no
// UTF-8 rewriting and no second base64 layer.
func TestRelayStreaming_BinaryChunksPassThroughUnchanged(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	ft := connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resCh := startSSE(srv, env.token1, "/relay/"+env.device1.Subdomain+"/event", ctx)

	req := ft.nextRequest(t)
	ft.write(t, streamHead(req.ID, map[string]string{
		"Content-Type": "text/event-stream",
		"X-E2E-Stream": "1",
	}))

	resp := (<-resCh).must(t)
	defer resp.Body.Close()
	if got := resp.Header.Get("X-E2E-Stream"); got != "1" {
		t.Fatalf("X-E2E-Stream = %q, want 1", got)
	}

	// Several chunks, each a valid AES-GCM sized payload, including binary
	// bytes and a chunk that starts and ends with NUL.
	parts := [][]byte{
		binaryPayload(4096),
		{0x00, 0xff, 0x00},
		binaryPayload(28 * 1024),
	}
	for i, part := range parts {
		ft.write(t, streamChunkBytes(req.ID, i, part))
	}
	ft.write(t, map[string]any{"type": "res-end", "id": req.ID})

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read stream body: %v", err)
	}
	want := bytes.Join(parts, nil)
	if !bytes.Equal(body, want) {
		t.Fatalf("stream body mismatch: got %d bytes, want %d", len(body), len(want))
	}
	waitStreamCount(t, env.manager, 0)
}

// The WebSocket bridge must preserve binary payload bytes in both directions.
func TestRelayWebSocket_BinaryFramesPassThroughUnchanged(t *testing.T) {
	env := newRelayTestEnv(t)
	srv := httptest.NewServer(env.mux)
	t.Cleanup(srv.Close)

	ft := connectStreamTunnel(t, env.manager, env.device1.Subdomain)

	client := dialRelayWebSocket(t, srv, env.token1, "/relay/"+env.device1.Subdomain+"/pty", nil)
	id := acceptWS(t, ft)

	// mobile -> host
	upstream := binaryPayload(8 * 1024)
	if err := client.WriteMessage(websocket.BinaryMessage, upstream); err != nil {
		t.Fatalf("failed to write client frame: %v", err)
	}
	frame := ft.waitFrame(t, "ws-frame")
	opcode, data := wsFramePayload(t, frame)
	if opcode != websocket.BinaryMessage {
		t.Errorf("ws-frame opcode = %d, want %d", opcode, websocket.BinaryMessage)
	}
	if !bytes.Equal(data, upstream) {
		t.Fatalf("mobile -> host payload mismatch: got %d bytes, want %d", len(data), len(upstream))
	}

	// host -> mobile
	downstream := binaryPayload(6 * 1024)
	ft.write(t, map[string]any{
		"type":   "ws-frame",
		"id":     id,
		"opcode": websocket.BinaryMessage,
		"data":   base64.StdEncoding.EncodeToString(downstream),
	})
	client.SetReadDeadline(time.Now().Add(3 * time.Second))
	messageType, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read client frame: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Errorf("message type = %d, want %d", messageType, websocket.BinaryMessage)
	}
	if !bytes.Equal(payload, downstream) {
		t.Fatalf("host -> mobile payload mismatch: got %d bytes, want %d", len(payload), len(downstream))
	}
}
