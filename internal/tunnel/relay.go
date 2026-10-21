package tunnel

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/zero469/opencode-relay-server/internal/middleware"
	"github.com/zero469/opencode-relay-server/internal/models"
	"github.com/zero469/opencode-relay-server/internal/services"
)

// streamHeadTimeout bounds how long the relay waits for the host's res-head.
const streamHeadTimeout = 120 * time.Second

// RelayHandler serves the authenticated /relay/{hostId}/... surface. It lives
// in the tunnel package so it can reuse the unexported forwarding helper.
type RelayHandler struct {
	manager *Manager
	devices *services.DeviceService
}

func NewRelayHandler(manager *Manager, devices *services.DeviceService) *RelayHandler {
	return &RelayHandler{
		manager: manager,
		devices: devices,
	}
}

func (h *RelayHandler) HandleRelay(w http.ResponseWriter, r *http.Request) {
	hostID, targetPath := splitRelayPath(r.URL.Path)
	if hostID == "" {
		writeRelayError(w, "host required", http.StatusBadRequest)
		return
	}

	device, ok := h.resolveHost(w, middleware.GetUserID(r), hostID)
	if !ok {
		return
	}

	log.Printf("[relay] Incoming request: %s %q -> host=%s, targetPath=%q", r.Method, r.URL.Path, device.Subdomain, targetPath)
	if isWebSocketUpgrade(r) || acceptsEventStream(r) {
		if v := h.manager.ProtocolVersion(device.Subdomain); v < TunnelProtocolVersion {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotImplemented)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "tunnel_protocol_too_old",
				"message": "this host runs a legacy tunnel client; streaming and WebSocket need protocol 2",
			})
			return
		}
	}
	if isWebSocketUpgrade(r) {
		h.forwardWebSocket(w, r, device.Subdomain, targetPath)
		return
	}
	if acceptsEventStream(r) {
		h.forwardStream(w, r, device.Subdomain, targetPath)
		return
	}
	forwardHTTP(w, r, h.manager, device.Subdomain, targetPath)
}

// acceptsEventStream reports whether the client asked for a server-sent event
// stream, which must be relayed incrementally.
func acceptsEventStream(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mediaType := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if strings.EqualFold(mediaType, "text/event-stream") {
			return true
		}
	}
	return false
}

// forwardStream relays an SSE response chunk by chunk. The status line and
// headers are flushed as soon as res-head arrives, then every res-chunk is
// written and flushed immediately.
func (h *RelayHandler) forwardStream(w http.ResponseWriter, r *http.Request, hostID, targetPath string) {
	if !h.manager.IsConnected(hostID) {
		log.Printf("[forward] Device not connected: %s", hostID)
		http.Error(w, "device not connected", http.StatusServiceUnavailable)
		return
	}

	body, err := ReadRequestBody(r)
	if err != nil {
		log.Printf("[forward] Failed to read request body: %v", err)
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if len(body) > maxFrameBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	req := buildTunnelRequest(r, targetPath, body)
	stream, err := h.manager.ForwardStream(r.Context(), hostID, req)
	if err != nil {
		log.Printf("[forward] ForwardStream failed: %v", err)
		switch {
		case errors.Is(err, ErrTunnelNotFound):
			http.Error(w, "device not connected", http.StatusServiceUnavailable)
		case errors.Is(err, ErrTooManyStreams):
			http.Error(w, "too many concurrent streams", http.StatusTooManyRequests)
		case errors.Is(err, ErrFrameTooLarge):
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		default:
			http.Error(w, "tunnel error", http.StatusBadGateway)
		}
		return
	}

	select {
	case head := <-stream.Head:
		for key, value := range head.Headers {
			w.Header().Set(key, value)
		}
		status := head.StatusCode
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		flushResponse(w)
	case err := <-stream.Err:
		log.Printf("[forward] Stream failed before head: %v", err)
		http.Error(w, "tunnel stream error", http.StatusBadGateway)
		return
	case <-r.Context().Done():
		return
	case <-time.After(streamHeadTimeout):
		// The stream outlived the head wait: tell the host to stop working on
		// it instead of leaving it registered until the request context dies.
		stream.cancel()
		http.Error(w, "tunnel stream timeout", http.StatusGatewayTimeout)
		return
	}

	for {
		select {
		case chunk, ok := <-stream.Data:
			if !ok {
				if err := stream.ErrValue(); err != nil {
					log.Printf("[forward] Stream ended with error: %v", err)
				}
				return
			}
			// res-chunk payloads are opaque: they may be per-chunk AES-GCM
			// ciphertext (design D8). Never decode, trim or rewrite them.
			if _, err := w.Write(chunk); err != nil {
				log.Printf("[forward] Failed to write stream chunk: %v", err)
				return
			}
			flushResponse(w)
		case err := <-stream.Err:
			// The host reported a terminal error. Do not wait for Data to be
			// closed: the error is the authoritative end of the stream.
			log.Printf("[forward] Stream failed: %v", err)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func flushResponse(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func splitRelayPath(path string) (hostID, targetPath string) {
	parts := strings.SplitN(strings.TrimPrefix(path, "/relay/"), "/", 2)
	hostID = parts[0]
	targetPath = "/"
	if len(parts) > 1 {
		targetPath = "/" + parts[1]
	}
	return hostID, targetPath
}

func (h *RelayHandler) resolveHost(w http.ResponseWriter, userID int64, hostID string) (*models.Device, bool) {
	device, err := h.devices.ResolveHost(userID, hostID)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrHostForbidden):
			writeRelayError(w, "host forbidden", http.StatusForbidden)
		case errors.Is(err, services.ErrHostNotFound):
			writeRelayError(w, "host not found", http.StatusNotFound)
		default:
			writeRelayError(w, "failed to resolve host", http.StatusInternalServerError)
		}
		return nil, false
	}
	return device, true
}

func writeRelayError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
