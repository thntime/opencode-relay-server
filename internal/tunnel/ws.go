package tunnel

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

// wsAckTimeout bounds how long the relay waits for the host's ws-ack after the
// mobile client completed its handshake. It is a variable so tests can shorten
// it.
var wsAckTimeout = 10 * time.Second

const (
	// wsFrameBuffer is the per-stream buffer for host -> mobile frames. See
	// design D6 (32 frames or 4 MiB).
	wsFrameBuffer = 32
	// wsBufferBytes caps the buffered WebSocket payload per stream.
	wsBufferBytes = 4 << 20
)

// errWsPeerClosed marks a WebSocket session that ended because the peer closed
// it; the relay already exchanged close frames, so it must not send another
// one.
var errWsPeerClosed = errors.New("websocket peer closed")

// isWebSocketUpgrade reports whether r asks to upgrade the connection to
// WebSocket.
func isWebSocketUpgrade(r *http.Request) bool {
	if !headerHasToken(r.Header.Get("Connection"), "upgrade") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}

func headerHasToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// forwardWebSocket bridges a mobile-side WebSocket to the host through the
// tunnel. The mobile handshake is completed locally; the host is then asked to
// dial its own local endpoint with ws-open. Any failure after the handshake is
// reported with close code 1011 and a reason, because the HTTP status line is
// already on the wire.
func (h *RelayHandler) forwardWebSocket(w http.ResponseWriter, r *http.Request, hostID, targetPath string) {
	// Hijack (and therefore a WebSocket upgrade) requires HTTP/1.x.
	if r.ProtoMajor != 1 {
		http.Error(w, "websocket upgrade requires HTTP/1.1", http.StatusUpgradeRequired)
		return
	}
	if _, ok := w.(http.Hijacker); !ok {
		http.Error(w, "websocket upgrade requires HTTP/1.1", http.StatusUpgradeRequired)
		return
	}

	requested := websocket.Subprotocols(r)
	upgrader := websocket.Upgrader{
		CheckOrigin:     func(*http.Request) bool { return true },
		Subprotocols:    requested,
		ReadBufferSize:  64 << 10,
		WriteBufferSize: 64 << 10,
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[relay] websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	// Close frames are echoed explicitly (closeWith) so the relay can share a
	// single sync.Once; suppress gorilla's automatic reply to avoid a second
	// close frame on the wire.
	conn.SetCloseHandler(func(code int, text string) error { return nil })

	var closeOnce sync.Once
	closeWith := func(code int, reason string) {
		closeOnce.Do(func() {
			deadline := time.Now().Add(2 * time.Second)
			if err := conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, truncateCloseReason(reason)), deadline); err != nil {
				log.Printf("[relay] failed to send websocket close frame: %v", err)
			}
		})
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	id := GenerateRequestID()
	stream, err := h.manager.registerStream(ctx, hostID, id, streamWS)
	if err != nil {
		log.Printf("[relay] websocket stream registration failed: %v", err)
		code := websocket.CloseInternalServerErr
		if errors.Is(err, ErrTunnelNotFound) {
			code = websocket.CloseGoingAway
		}
		closeWith(code, wsFailureReason(err))
		return
	}
	// cancel is idempotent and sends exactly one req-cancel (which also
	// unregisters). It must be the only teardown path, otherwise the host's
	// local socket and pumpLocalWs leak on every abnormal exit.
	defer stream.cancel()

	open := &WsOpen{
		ID:           id,
		Path:         wsTargetPath(r, targetPath),
		Headers:      wsForwardHeaders(r),
		Subprotocols: requested,
	}
	if err := stream.SendFrame(open); err != nil {
		log.Printf("[relay] failed to send ws-open for stream %s: %v", id, err)
		closeWith(websocket.CloseInternalServerErr, wsFailureReason(err))
		return
	}

	var ack *WsAck
	select {
	case ack = <-stream.Ack:
		if !ack.OK {
			reason := ack.Error
			if reason == "" {
				reason = "host rejected websocket"
				if ack.Status != 0 {
					reason = "host rejected websocket (status " + strconv.Itoa(ack.Status) + ")"
				}
			}
			log.Printf("[relay] host rejected websocket %s: status=%d error=%q", id, ack.Status, ack.Error)
			closeWith(websocket.CloseInternalServerErr, reason)
			return
		}
	case err := <-stream.Err:
		log.Printf("[relay] websocket stream %s failed before ack: %v", id, err)
		closeWith(websocket.CloseInternalServerErr, wsFailureReason(err))
		return
	case <-stream.done:
		err := stream.ErrValue()
		log.Printf("[relay] websocket stream %s closed before ack: %v", id, err)
		closeWith(websocket.CloseInternalServerErr, wsFailureReason(err))
		return
	case <-time.After(wsAckTimeout):
		log.Printf("[relay] ws-ack timeout for stream %s", id)
		closeWith(websocket.CloseInternalServerErr, "host did not acknowledge websocket")
		return
	case <-ctx.Done():
		return
	}

	// The mobile handshake already told the client which subprotocol was
	// selected locally. If the host negotiated a different one, the two ends
	// would speak different protocols on the same stream.
	if ack.Subprotocol != "" && ack.Subprotocol != conn.Subprotocol() {
		log.Printf("[relay] websocket %s subprotocol mismatch: host=%q client=%q", id, ack.Subprotocol, conn.Subprotocol())
		closeWith(websocket.CloseInternalServerErr, "subprotocol mismatch: host negotiated "+ack.Subprotocol+", client requested "+conn.Subprotocol())
		return
	}

	results := make(chan error, 2)
	go pumpClientToHost(conn, stream, results, closeWith)
	go pumpHostToClient(conn, stream, results, closeWith)

	err = <-results
	switch {
	case err == nil, errors.Is(err, errWsPeerClosed):
		// Close frames were already exchanged.
	case errors.Is(err, ErrFrameTooLarge):
		closeWith(websocket.CloseMessageTooBig, "frame too large")
	default:
		log.Printf("[relay] websocket stream %s ended: %v", id, err)
		closeWith(websocket.CloseInternalServerErr, wsFailureReason(err))
	}
}

// pumpClientToHost forwards data frames from the mobile client to the host.
// Ping/pong are answered by the local gorilla connection and never cross the
// tunnel. A client-initiated close is echoed to the mobile peer as well as
// forwarded to the host so both sides observe the same handshake.
func pumpClientToHost(conn *websocket.Conn, s *Stream, results chan<- error, closeWith func(int, string)) {
	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			// 1006 is a local-only placeholder and must never be written to
			// the wire (RFC 6455 §7.4), so transport failures map to 1011.
			code := websocket.CloseInternalServerErr
			reason := err.Error()
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				code = closeErr.Code
				reason = closeErr.Text
			}
			_ = s.SendFrame(&WsClose{ID: s.ID, Code: code, Reason: truncateCloseReason(reason)})
			// Echo the close back to the mobile client: the peer expects a
			// close frame before the TCP connection is torn down.
			closeWith(code, reason)
			results <- errWsPeerClosed
			return
		}

		if len(data) > maxFrameBytes {
			log.Printf("[relay] websocket frame too large from client (%d bytes), closing stream %s", len(data), s.ID)
			_ = s.SendFrame(&WsClose{ID: s.ID, Code: websocket.CloseMessageTooBig, Reason: "frame too large"})
			results <- ErrFrameTooLarge
			return
		}

		// Control frames are handled by gorilla inside ReadMessage, so only
		// data frames reach this point. The payload is opaque: it may already
		// be encrypted by the mobile client (design D8) and must not be
		// decoded or rewritten here.
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}
		if err := s.SendFrame(&WsFrame{ID: s.ID, Opcode: messageType, Data: data}); err != nil {
			results <- err
			return
		}
	}
}

// pumpHostToClient forwards frames from the host to the mobile client and
// propagates close codes in both directions. Close is checked first and is
// never subject to the ws-frame backpressure path.
func pumpHostToClient(conn *websocket.Conn, s *Stream, results chan<- error, closeWith func(int, string)) {
	handleClose := func(f *WsClose) {
		code := f.Code
		if code == 0 {
			code = websocket.CloseNormalClosure
		}
		closeWith(code, f.Reason)
		results <- errWsPeerClosed
	}

	for {
		select {
		case f := <-s.Close:
			handleClose(f)
			return
		default:
		}

		select {
		case f := <-s.Close:
			handleClose(f)
			return
		case frame := <-s.Frames:
			switch f := frame.(type) {
			case *WsFrame:
				s.wsBytes.Add(-int64(len(f.Data)))
				if len(f.Data) > maxFrameBytes {
					log.Printf("[relay] websocket frame too large from host (%d bytes), closing stream %s", len(f.Data), s.ID)
					closeWith(websocket.CloseMessageTooBig, "frame too large")
					results <- ErrFrameTooLarge
					return
				}
				opcode := f.Opcode
				if opcode != websocket.TextMessage && opcode != websocket.BinaryMessage {
					opcode = websocket.BinaryMessage
				}
				if err := conn.WriteMessage(opcode, f.Data); err != nil {
					results <- err
					return
				}
			case *WsClose:
				// Older deliveries could still arrive through Frames.
				handleClose(f)
				return
			}
		case <-s.done:
			if err := s.ErrValue(); err != nil {
				log.Printf("[relay] websocket stream %s ended: %v", s.ID, err)
				closeWith(websocket.CloseInternalServerErr, wsFailureReason(err))
				results <- err
			} else {
				closeWith(websocket.CloseNormalClosure, "")
				results <- errWsPeerClosed
			}
			return
		}
	}
}

// wsTargetPath appends the original query string to the relay target path.
func wsTargetPath(r *http.Request, targetPath string) string {
	if r.URL.RawQuery == "" {
		return targetPath
	}
	return targetPath + "?" + r.URL.RawQuery
}

// wsForwardHeaders snapshots the headers the host needs to dial its local
// endpoint. Relay credentials, hop-by-hop headers and the client's own
// handshake headers are not forwarded; the tunnel client generates its own
// handshake.
func wsForwardHeaders(r *http.Request) map[string]string {
	headers := make(map[string]string)
	for key := range r.Header {
		if isHopByHopHeader(key) || isRelayCredentialHeader(key) {
			continue
		}
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "sec-websocket-") {
			continue
		}
		// Relay-internal protocol markers must not leak into the host's
		// local handshake.
		if lower == "x-tunnel-protocol" || lower == "x-e2e-stream" {
			continue
		}
		headers[key] = r.Header.Get(key)
	}
	return headers
}

// wsFailureReason turns a terminal stream error into a human-readable close
// reason for the mobile client.
func wsFailureReason(err error) string {
	switch {
	case err == nil:
		return "tunnel stream closed"
	case errors.Is(err, ErrTunnelNotFound):
		return "device not connected"
	case errors.Is(err, ErrStreamBackpressure):
		return "stream buffer full"
	case errors.Is(err, ErrSlowConsumer):
		return "stream slow consumer"
	default:
		return err.Error()
	}
}

// truncateCloseReason keeps a close reason inside the 125-byte control frame
// limit (123 bytes plus the 2-byte code) without splitting a UTF-8 rune.
func truncateCloseReason(reason string) string {
	const maxReason = 123
	if len(reason) <= maxReason {
		return reason
	}
	for i := maxReason; i > 0; i-- {
		if utf8.RuneStart(reason[i]) {
			return reason[:i]
		}
	}
	return ""
}
