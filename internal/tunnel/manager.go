package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// TunnelProtocolVersion is the highest tunnel protocol the relay speaks.
// Version 2 adds streaming responses and WebSocket passthrough.
const TunnelProtocolVersion = 2

var (
	ErrTunnelNotFound = errors.New("tunnel not found")
	ErrTunnelTimeout  = errors.New("tunnel request timeout")

	// ErrTooManyStreams is returned when a connection already has
	// maxStreamsPerConn active streams.
	ErrTooManyStreams = errors.New("too many concurrent streams")
	// ErrStreamExists is returned when a stream ID is already registered.
	ErrStreamExists = errors.New("stream already exists")
	// ErrStreamBackpressure is set on a stream whose consumer stopped
	// draining the Data channel.
	ErrStreamBackpressure = errors.New("stream buffer full")
	// ErrSlowConsumer is set on a stream that made no progress for
	// slowConsumerTimeout.
	ErrSlowConsumer = errors.New("stream slow consumer")
)

const (
	// maxStreamsPerConn caps concurrent streams per tunnel connection.
	maxStreamsPerConn = 16
	// streamBuffer is the per-stream chunk buffer. See design D6.
	streamBuffer = 32
	// defaultSlowConsumerTimeout is how long a stream may make no progress
	// while data is pending before it is aborted.
	defaultSlowConsumerTimeout = 30 * time.Second
)

// streamKind distinguishes the two streaming consumers so only SSE streams pay
// for the slow-consumer watcher.
type streamKind int

const (
	streamSSE streamKind = iota
	streamWS
)

type TunnelRequest struct {
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    []byte            `json:"body"`
}

type TunnelResponse struct {
	ID         string            `json:"id"`
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	Body       []byte            `json:"body"`
}

type TunnelEvent struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

type HeartbeatMessage struct {
	Type string `json:"type"` // "heartbeat" or "heartbeat_ack"
	Ts   int64  `json:"ts"`   // Unix milliseconds
}

type TunnelConnection struct {
	conn      *websocket.Conn
	subdomain string
	pending   map[string]chan *TunnelResponse
	pendingMu sync.RWMutex
	writeMu   sync.Mutex
	closeChan chan struct{}
	closeOnce sync.Once
	replaced  bool

	// streamCount is guarded by Manager.streamsMu.
	streamCount int

	// proto is the tunnel protocol version negotiated at handshake: 1 for
	// legacy clients that do not announce X-Tunnel-Protocol.
	proto int
}

// Stream is one in-flight streaming response. Head receives the first
// res-head frame exactly once; Data carries res-chunk payloads in arrival
// order; Err receives a terminal error (if any). Data is closed when the
// stream ends, normally or not.
type Stream struct {
	ID     string
	Head   chan *TunnelResponse
	Data   chan []byte
	Err    chan error
	cancel context.CancelFunc

	// Ack receives the host's single ws-ack for a WebSocket stream.
	Ack chan *WsAck
	// Frames receives ws-frame and ws-close values for a WebSocket stream.
	// It is never closed; done signals the end of the stream.
	Frames chan any
	// Close receives at most one host ws-close. It is kept separate from
	// Frames so backpressure can never drop a close frame.
	Close chan *WsClose

	conn    *TunnelConnection
	manager *Manager

	headOnce   sync.Once
	closeOnce  sync.Once
	cancelOnce sync.Once

	done         chan struct{}
	mu           sync.RWMutex
	err          error
	finished     bool
	lastSeq      int64
	lastProgress atomic.Int64
	wsBytes      atomic.Int64
}

type EventClient struct {
	conn      *websocket.Conn
	subdomain string
	closeChan chan struct{}
	closeOnce sync.Once
	writeMu   sync.Mutex
}

type Manager struct {
	connections  map[string]*TunnelConnection
	eventClients map[string]map[*EventClient]struct{}
	mu           sync.RWMutex
	upgrader     websocket.Upgrader
	onHeartbeat  func(subdomain string)
	onDisconnect func(subdomain string)

	streams   map[string]*Stream
	streamsMu sync.RWMutex

	// slowConsumerTimeout aborts a stream that stops draining its Data
	// channel. It is per-Manager so tests do not race on a package global.
	slowConsumerTimeout time.Duration
}

func NewManager() *Manager {
	return &Manager{
		connections:         make(map[string]*TunnelConnection),
		eventClients:        make(map[string]map[*EventClient]struct{}),
		streams:             make(map[string]*Stream),
		slowConsumerTimeout: defaultSlowConsumerTimeout,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
			ReadBufferSize:    1024 * 64,
			WriteBufferSize:   1024 * 64,
			EnableCompression: false,
		},
	}
}

func (m *Manager) SetCallbacks(onHeartbeat, onDisconnect func(subdomain string)) {
	m.onHeartbeat = onHeartbeat
	m.onDisconnect = onDisconnect
}

func (m *Manager) HandleWebSocket(w http.ResponseWriter, r *http.Request, subdomain string) error {
	proto := 1
	if v, err := strconv.Atoi(strings.TrimSpace(r.Header.Get("X-Tunnel-Protocol"))); err == nil && v >= TunnelProtocolVersion {
		proto = TunnelProtocolVersion
	}
	// The version must travel in the handshake response headers; the client
	// reads it there to decide between streaming and legacy framing.
	responseHeader := http.Header{}
	responseHeader.Set("X-Tunnel-Protocol", strconv.Itoa(proto))

	conn, err := m.upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		return err
	}

	// Set initial read deadline for Azure compatibility
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	// Set pong handler to extend read deadline when pong received
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	// Set ping handler to extend read deadline when ping received from client
	conn.SetPingHandler(func(appData string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		// Write pong with appData (gorilla requires manual pong when using custom PingHandler)
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
	})

	tc := &TunnelConnection{
		conn:      conn,
		subdomain: subdomain,
		pending:   make(map[string]chan *TunnelResponse),
		closeChan: make(chan struct{}),
		proto:     proto,
	}

	m.mu.Lock()
	if existing, ok := m.connections[subdomain]; ok {
		existing.replaced = true
		existing.Close()
	}
	m.connections[subdomain] = tc
	m.mu.Unlock()

	log.Printf("[tunnel] Client connected: %s", subdomain)

	if m.onHeartbeat != nil {
		go m.onHeartbeat(subdomain)
	}

	go tc.readLoop(m)
	go tc.pingLoop(m)

	// Block until connection closes — required for Azure App Service compatibility
	<-tc.closeChan
	return nil
}

func (m *Manager) ForwardRequest(subdomain string, req *TunnelRequest) (*TunnelResponse, error) {
	m.mu.RLock()
	tc, ok := m.connections[subdomain]
	m.mu.RUnlock()

	if !ok {
		return nil, ErrTunnelNotFound
	}

	return tc.SendRequest(req)
}

// ForwardStream registers a streaming request and writes it to the host. The
// caller reads Head once, then drains Data until it is closed. Cancelling ctx
// (or calling Stream.cancel) sends one req-cancel to the host and unregisters
// the stream.
func (m *Manager) ForwardStream(ctx context.Context, hostID string, req *TunnelRequest) (*Stream, error) {
	s, err := m.registerStream(ctx, hostID, req.ID, streamSSE)
	if err != nil {
		return nil, err
	}

	if err := s.conn.writeJSON(req); err != nil {
		m.unregisterStream(req.ID)
		return nil, err
	}
	return s, nil
}

// registerStream registers a stream and starts its cancellation watcher
// without sending anything to the host. The WebSocket bridge uses it to follow
// up with a ws-open frame. Only SSE streams get a slow-consumer watcher.
func (m *Manager) registerStream(ctx context.Context, hostID, id string, kind streamKind) (*Stream, error) {
	m.mu.RLock()
	tc := m.connections[hostID]
	m.mu.RUnlock()
	if tc == nil {
		return nil, ErrTunnelNotFound
	}

	m.streamsMu.Lock()
	if tc.streamCount >= maxStreamsPerConn {
		m.streamsMu.Unlock()
		return nil, ErrTooManyStreams
	}
	if _, exists := m.streams[id]; exists {
		m.streamsMu.Unlock()
		return nil, ErrStreamExists
	}
	s := newStream(id, tc, m)
	m.streams[id] = s
	tc.streamCount++
	m.streamsMu.Unlock()

	go s.watchContext(ctx)
	if kind != streamWS {
		go s.watchSlowConsumer(m.slowConsumerTimeout)
	}
	return s, nil
}

// StreamCount reports the number of active streams across all connections.
func (m *Manager) StreamCount() int {
	m.streamsMu.RLock()
	defer m.streamsMu.RUnlock()
	return len(m.streams)
}

func newStream(id string, tc *TunnelConnection, m *Manager) *Stream {
	s := &Stream{
		ID:      id,
		Head:    make(chan *TunnelResponse, 1),
		Data:    make(chan []byte, streamBuffer),
		Err:     make(chan error, 1),
		Ack:     make(chan *WsAck, 1),
		Frames:  make(chan any, wsFrameBuffer),
		Close:   make(chan *WsClose, 1),
		conn:    tc,
		manager: m,
		done:    make(chan struct{}),
		lastSeq: -1,
	}
	s.cancel = s.cancelStream
	s.lastProgress.Store(time.Now().UnixNano())
	return s
}

func (m *Manager) lookupStream(id string) *Stream {
	m.streamsMu.RLock()
	s := m.streams[id]
	m.streamsMu.RUnlock()
	return s
}

// unregisterStream removes id from the stream map and closes its channels. It
// is idempotent.
func (m *Manager) unregisterStream(id string) {
	m.streamsMu.Lock()
	s, ok := m.streams[id]
	if ok {
		delete(m.streams, id)
		if s.conn != nil && s.conn.streamCount > 0 {
			s.conn.streamCount--
		}
	}
	m.streamsMu.Unlock()

	if ok {
		s.finish(nil)
	}
}

// failStream terminates a stream with err and unregisters it.
func (m *Manager) failStream(id string, err error) {
	s := m.lookupStream(id)
	if s == nil {
		return
	}
	s.setErr(err)
	select {
	case s.Err <- err:
	default:
	}
	m.unregisterStream(id)
}

// abortStream terminates a stream due to a local resource limit and tells the
// host to stop working on it.
func (m *Manager) abortStream(s *Stream, err error) {
	s.setErr(err)
	select {
	case s.Err <- err:
	default:
	}
	s.cancelStream()
}

// failStreams terminates every stream bound to tc, e.g. after the tunnel
// connection drops.
func (m *Manager) failStreams(tc *TunnelConnection, err error) {
	m.streamsMu.RLock()
	ids := make([]string, 0, len(m.streams))
	for id, s := range m.streams {
		if s.conn == tc {
			ids = append(ids, id)
		}
	}
	m.streamsMu.RUnlock()

	for _, id := range ids {
		m.failStream(id, err)
	}
}

func (s *Stream) finish(err error) {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if err != nil && s.err == nil {
			s.err = err
		}
		s.finished = true
		s.mu.Unlock()
		close(s.done)
		close(s.Data)
	})
}

func (s *Stream) setErr(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// ErrValue returns the terminal error, or nil when the stream ended normally.
func (s *Stream) ErrValue() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}

func (s *Stream) isFinished() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.finished
}

// pushData enqueues a chunk without blocking. It reports false when the
// stream is finished or the buffer is full.
func (s *Stream) pushData(data []byte) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.finished {
		return false
	}
	select {
	case s.Data <- data:
		s.lastProgress.Store(time.Now().UnixNano())
		return true
	default:
		return false
	}
}

// SendFrame writes one frame to the host connection that owns the stream.
func (s *Stream) SendFrame(frame any) error {
	if s.conn == nil {
		return ErrTunnelNotFound
	}
	return s.conn.writeFrame(frame)
}

// cancelStream sends one req-cancel to the host and unregisters the stream.
func (s *Stream) cancelStream() {
	s.cancelOnce.Do(func() {
		if s.isFinished() {
			return
		}
		if s.conn != nil {
			if err := s.SendFrame(&ReqCancel{ID: s.ID}); err != nil {
				log.Printf("[tunnel] Failed to send req-cancel for stream %s: %v", s.ID, err)
			}
		}
		log.Printf("[tunnel] Stream canceled: %s", s.ID)
		if s.manager != nil {
			s.manager.unregisterStream(s.ID)
		} else {
			s.finish(nil)
		}
	})
}

func (s *Stream) watchContext(ctx context.Context) {
	select {
	case <-s.done:
		return
	case <-ctx.Done():
	}
	// A normal end may race with context cancellation; do not send a
	// req-cancel for a stream that already finished.
	if s.isFinished() {
		return
	}
	s.cancelStream()
}

func (s *Stream) watchSlowConsumer(timeout time.Duration) {
	interval := timeout / 4
	if interval <= 0 {
		interval = timeout
	}
	if interval <= 0 {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if len(s.Data) == 0 {
				continue
			}
			last := time.Unix(0, s.lastProgress.Load())
			if time.Since(last) < timeout {
				continue
			}
			if s.manager != nil {
				s.manager.abortStream(s, ErrSlowConsumer)
			} else {
				s.cancelStream()
			}
			return
		}
	}
}

// deliverHead forwards the first res-head for a stream. Duplicate heads are
// ignored.
func (m *Manager) deliverHead(f *ResHead) {
	s := m.lookupStream(f.ID)
	if s == nil {
		return
	}
	s.headOnce.Do(func() {
		s.mu.RLock()
		defer s.mu.RUnlock()
		if s.finished {
			return
		}
		select {
		case s.Head <- &TunnelResponse{ID: f.ID, StatusCode: f.StatusCode, Headers: f.Headers}:
		default:
		}
	})
}

func (m *Manager) deliverChunk(f *ResChunk) {
	s := m.lookupStream(f.ID)
	if s == nil {
		return
	}
	if s.pushData(f.Data) {
		s.mu.Lock()
		s.lastSeq = f.Seq
		s.mu.Unlock()
		return
	}
	if s.isFinished() {
		return
	}
	log.Printf("[tunnel] Stream %s buffer full, aborting", f.ID)
	m.abortStream(s, ErrStreamBackpressure)
}

// handleFrame dispatches typed frames received from a tunnel client. Legacy
// *TunnelResponse frames are handled by the caller so the whole-package
// pending path stays untouched.
func (m *Manager) handleFrame(frame any) {
	switch f := frame.(type) {
	case *ResHead:
		m.deliverHead(f)
	case *ResChunk:
		m.deliverChunk(f)
	case *ResEnd:
		m.unregisterStream(f.ID)
	case *ResError:
		m.failStream(f.ID, errors.New(f.Message))
	case *WsAck:
		m.deliverWsAck(f)
	case *WsFrame:
		m.deliverWsFrame(f)
	case *WsClose:
		m.deliverWsClose(f)
	default:
		// Unknown typed frames are ignored, but log the type so a protocol
		// mismatch is diagnosable instead of silent.
		log.Printf("[tunnel] Ignoring unknown frame type %T", frame)
	}
}

// deliverWsAck hands the host's ws-ack to the WebSocket bridge. Duplicate acks
// are dropped.
func (m *Manager) deliverWsAck(f *WsAck) {
	s := m.lookupStream(f.ID)
	if s == nil {
		return
	}
	select {
	case s.Ack <- f:
	default:
	}
}

// deliverWsFrame enqueues a host frame without blocking. It aborts the stream
// when the mobile side falls behind (design D6).
func (m *Manager) deliverWsFrame(f *WsFrame) {
	s := m.lookupStream(f.ID)
	if s == nil || s.isFinished() {
		return
	}

	size := int64(len(f.Data))
	if s.wsBytes.Add(size) > wsBufferBytes {
		s.wsBytes.Add(-size)
		log.Printf("[tunnel] Stream %s websocket buffer full, aborting", f.ID)
		m.abortStream(s, ErrStreamBackpressure)
		return
	}

	select {
	case s.Frames <- f:
		s.lastProgress.Store(time.Now().UnixNano())
	default:
		s.wsBytes.Add(-size)
		log.Printf("[tunnel] Stream %s websocket buffer full, aborting", f.ID)
		m.abortStream(s, ErrStreamBackpressure)
	}
}

// deliverWsClose forwards the host's close frame to the bridge. Close is never
// dropped for backpressure: when its slot is occupied the stale value is
// replaced, and only a truly stuck stream is aborted.
func (m *Manager) deliverWsClose(f *WsClose) {
	s := m.lookupStream(f.ID)
	if s == nil || s.isFinished() {
		return
	}
	select {
	case s.Close <- f:
		s.lastProgress.Store(time.Now().UnixNano())
		return
	default:
	}
	select {
	case <-s.Close:
	default:
	}
	select {
	case s.Close <- f:
		s.lastProgress.Store(time.Now().UnixNano())
	default:
		log.Printf("[tunnel] Stream %s close buffer unavailable, aborting", f.ID)
		m.abortStream(s, ErrStreamBackpressure)
	}
}

// ProtocolVersion reports the negotiated tunnel protocol for a host: 1 when the
// connection predates version negotiation or the host is offline.
func (m *Manager) ProtocolVersion(subdomain string) int {
	m.mu.RLock()
	tc, ok := m.connections[subdomain]
	m.mu.RUnlock()
	if !ok || tc.proto == 0 {
		return 1
	}
	return tc.proto
}

func (m *Manager) IsConnected(subdomain string) bool {
	m.mu.RLock()
	_, ok := m.connections[subdomain]
	m.mu.RUnlock()
	return ok
}

func (m *Manager) removeConnection(tc *TunnelConnection) {
	m.mu.Lock()
	current, ok := m.connections[tc.subdomain]
	if ok && current == tc {
		delete(m.connections, tc.subdomain)
	}
	m.mu.Unlock()

	if tc.replaced {
		log.Printf("[tunnel] Client replaced: %s (skipping disconnect callback)", tc.subdomain)
		return
	}

	log.Printf("[tunnel] Client disconnected: %s", tc.subdomain)

	if m.onDisconnect != nil {
		m.onDisconnect(tc.subdomain)
	}
}

func (tc *TunnelConnection) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	tc.writeMu.Lock()
	defer tc.writeMu.Unlock()
	tc.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return tc.conn.WriteMessage(websocket.TextMessage, data)
}

func (tc *TunnelConnection) writeFrame(frame any) error {
	data, err := encodeFrame(frame)
	if err != nil {
		return err
	}

	tc.writeMu.Lock()
	defer tc.writeMu.Unlock()
	tc.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return tc.conn.WriteMessage(websocket.TextMessage, data)
}

func (tc *TunnelConnection) SendRequest(req *TunnelRequest) (*TunnelResponse, error) {
	respChan := make(chan *TunnelResponse, 1)

	tc.pendingMu.Lock()
	tc.pending[req.ID] = respChan
	log.Printf("[debug] Registered pending request: ID=%s, Method=%s, Path=%s", req.ID, req.Method, req.Path)
	tc.pendingMu.Unlock()

	defer func() {
		tc.pendingMu.Lock()
		delete(tc.pending, req.ID)
		log.Printf("[debug] Removed pending request: ID=%s", req.ID)
		tc.pendingMu.Unlock()
	}()

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	log.Printf("[debug] Sending request to tunnel-client: %d bytes", len(data))

	tc.writeMu.Lock()
	tc.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	err = tc.conn.WriteMessage(websocket.TextMessage, data)
	tc.writeMu.Unlock()

	if err != nil {
		log.Printf("[debug] Failed to send request: %v", err)
		return nil, err
	}

	log.Printf("[debug] Request sent, waiting for response (timeout=120s)")

	select {
	case resp := <-respChan:
		log.Printf("[debug] Received response from channel: StatusCode=%d", resp.StatusCode)
		return resp, nil
	case <-time.After(120 * time.Second):
		log.Printf("[debug] Request timeout for ID=%s", req.ID)
		return nil, ErrTunnelTimeout
	case <-tc.closeChan:
		log.Printf("[debug] Connection closed while waiting for ID=%s", req.ID)
		return nil, ErrTunnelNotFound
	}
}

func (tc *TunnelConnection) readLoop(m *Manager) {
	defer func() {
		m.failStreams(tc, ErrTunnelNotFound)
		tc.Close()
		m.removeConnection(tc)
	}()

	for {
		_, message, err := tc.conn.ReadMessage()
		if err != nil {
			// ALWAYS log the error for debugging
			log.Printf("[tunnel] readLoop exit for %s: %v", tc.subdomain, err)
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("[tunnel] Read error for %s: %v", tc.subdomain, err)
			}
			return
		}

		// Reset read deadline on every successful read to prevent timeout
		tc.conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		if len(message) == 0 {
			continue
		}

		var heartbeatAck HeartbeatMessage
		if err := json.Unmarshal(message, &heartbeatAck); err == nil && heartbeatAck.Type == "heartbeat_ack" {
			continue
		}

		var tunnelEvent TunnelEvent
		if err := json.Unmarshal(message, &tunnelEvent); err == nil && tunnelEvent.Event == "sse" {
			m.broadcastEvent(tc.subdomain, message)
			continue
		}

		frame, err := decodeFrame(message)
		if err != nil {
			if !errors.Is(err, ErrUnknownFrame) {
				log.Printf("[tunnel] Failed to decode frame: %v (raw: %s)", err, string(message))
			}
			continue
		}

		resp, legacy := frame.(*TunnelResponse)
		if !legacy {
			m.handleFrame(frame)
			continue
		}

		log.Printf("[debug] Parsed response: ID=%s, StatusCode=%d, BodyLen=%d", resp.ID, resp.StatusCode, len(resp.Body))

		tc.pendingMu.RLock()
		pendingKeys := make([]string, 0, len(tc.pending))
		for k := range tc.pending {
			pendingKeys = append(pendingKeys, k)
		}
		log.Printf("[debug] Pending request IDs: %v", pendingKeys)

		if ch, ok := tc.pending[resp.ID]; ok {
			log.Printf("[debug] Found pending channel for ID=%s, sending response", resp.ID)
			select {
			case ch <- resp:
				log.Printf("[debug] Response sent to channel for ID=%s", resp.ID)
			default:
				log.Printf("[debug] Channel full/closed for ID=%s, response dropped", resp.ID)
			}
		} else {
			log.Printf("[debug] No pending request found for ID=%s", resp.ID)
		}
		tc.pendingMu.RUnlock()
	}
}

func (tc *TunnelConnection) pingLoop(m *Manager) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			heartbeat := HeartbeatMessage{
				Type: "heartbeat",
				Ts:   time.Now().UnixMilli(),
			}
			data, err := json.Marshal(heartbeat)
			if err != nil {
				log.Printf("[tunnel] Failed to marshal heartbeat: %v", err)
				return
			}

			tc.writeMu.Lock()
			tc.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err = tc.conn.WriteMessage(websocket.TextMessage, data)
			tc.writeMu.Unlock()
			if err != nil {
				log.Printf("[tunnel] Failed to send heartbeat to %s: %v", tc.subdomain, err)
				return
			}
			if m.onHeartbeat != nil {
				go m.onHeartbeat(tc.subdomain)
			}
		case <-tc.closeChan:
			return
		}
	}
}

func (tc *TunnelConnection) Close() {
	tc.closeOnce.Do(func() {
		close(tc.closeChan)
		tc.conn.Close()
	})
}

func ReadRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

func GenerateRequestID() string {
	// The timestamp alone can collide within one nanosecond under load; the
	// random suffix keeps stream IDs unique without a global counter.
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return time.Now().Format("20060102150405.000000000")
	}
	return time.Now().Format("20060102150405.000000000") + "-" + hex.EncodeToString(suffix[:])
}

func (m *Manager) HandleEventWebSocket(w http.ResponseWriter, r *http.Request, subdomain string) error {
	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return err
	}

	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	// Set ping handler to extend read deadline when ping received from client
	conn.SetPingHandler(func(appData string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		// Write pong with appData (gorilla requires manual pong when using custom PingHandler)
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
	})

	client := &EventClient{
		conn:      conn,
		subdomain: subdomain,
		closeChan: make(chan struct{}),
	}

	m.mu.Lock()
	if m.eventClients[subdomain] == nil {
		m.eventClients[subdomain] = make(map[*EventClient]struct{})
	}
	m.eventClients[subdomain][client] = struct{}{}
	clientCount := len(m.eventClients[subdomain])
	m.mu.Unlock()

	log.Printf("[events] Client connected for %s (total: %d)", subdomain, clientCount)

	go client.readLoop(m)
	go client.pingLoop()

	// Block until connection closes — required for handler lifecycle
	<-client.closeChan
	return nil
}

func (m *Manager) broadcastEvent(subdomain string, data []byte) {
	m.mu.RLock()
	clients := m.eventClients[subdomain]
	if clients == nil || len(clients) == 0 {
		m.mu.RUnlock()
		return
	}

	clientList := make([]*EventClient, 0, len(clients))
	for client := range clients {
		clientList = append(clientList, client)
	}
	m.mu.RUnlock()

	for _, client := range clientList {
		client.writeMu.Lock()
		err := client.conn.WriteMessage(websocket.TextMessage, data)
		client.writeMu.Unlock()
		if err != nil {
			log.Printf("[events] Failed to send to client: %v", err)
			client.Close()
			m.removeEventClient(client)
		}
	}
}

func (m *Manager) removeEventClient(client *EventClient) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if clients, ok := m.eventClients[client.subdomain]; ok {
		delete(clients, client)
		if len(clients) == 0 {
			delete(m.eventClients, client.subdomain)
		}
		log.Printf("[events] Client removed for %s (remaining: %d)", client.subdomain, len(clients))
	}
}

func (ec *EventClient) readLoop(m *Manager) {
	defer func() {
		ec.Close()
		m.removeEventClient(ec)
	}()

	for {
		_, _, err := ec.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("[events] Read error for %s: %v", ec.subdomain, err)
			}
			return
		}

		// Reset read deadline on every successful read to prevent timeout
		ec.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	}
}

func (ec *EventClient) pingLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ec.writeMu.Lock()
			ec.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err := ec.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			ec.writeMu.Unlock()
			if err != nil {
				return
			}
		case <-ec.closeChan:
			return
		}
	}
}

func (ec *EventClient) Close() {
	ec.closeOnce.Do(func() {
		close(ec.closeChan)
		ec.conn.Close()
	})
}
