package tunnel

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/zero469/opencode-relay-server/internal/database"
)

type Handler struct {
	manager     *Manager
	db          database.Database
	legacyProxy bool
}

func NewHandler(manager *Manager, db database.Database, legacyProxy bool) *Handler {
	manager.SetCallbacks(
		func(subdomain string) {
			start := time.Now()
			device, err := db.GetDeviceBySubdomain(subdomain)
			if err != nil {
				log.Printf("[tunnel] heartbeat: GetDeviceBySubdomain failed for %s: %v", subdomain, err)
				return
			}
			if err := db.UpdateDeviceHeartbeat(device.ID); err != nil {
				log.Printf("[tunnel] heartbeat: UpdateDeviceHeartbeat failed for %s (id=%d): %v", subdomain, device.ID, err)
			}
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				log.Printf("[tunnel] heartbeat: slow DB for %s: %v", subdomain, elapsed)
			}
		},
		func(subdomain string) {
			device, err := db.GetDeviceBySubdomain(subdomain)
			if err != nil {
				log.Printf("[tunnel] onDisconnect: GetDeviceBySubdomain failed for %s: %v", subdomain, err)
				return
			}
			if err := db.MarkDeviceOffline(device.ID); err != nil {
				log.Printf("[tunnel] onDisconnect: MarkDeviceOffline failed for %s (id=%d): %v", subdomain, device.ID, err)
			} else {
				log.Printf("[tunnel] onDisconnect: Device %s (id=%d) marked offline", subdomain, device.ID)
			}
		},
	)

	return &Handler{
		manager:     manager,
		db:          db,
		legacyProxy: legacyProxy,
	}
}

func (h *Handler) HandleTunnelConnect(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	parts := strings.Split(path, "/")
	if len(parts) < 4 {
		http.Error(w, `{"error":"subdomain_required","message":"Subdomain is required"}`, http.StatusBadRequest)
		return
	}
	subdomain := parts[3]

	device, err := h.db.GetDeviceBySubdomain(subdomain)
	if err != nil {
		log.Printf("[tunnel] Device not found: %s", subdomain)
		http.Error(w, `{"error":"device_not_found","message":"Device not found. Please re-pair your device."}`, http.StatusNotFound)
		return
	}

	authUser := r.URL.Query().Get("auth_user")
	authPassword := r.URL.Query().Get("auth_password")

	if authUser != device.AuthUser || authPassword != device.AuthPassword {
		log.Printf("[tunnel] Auth failed for device: %s", subdomain)
		http.Error(w, `{"error":"auth_failed","message":"Authentication failed. Please re-pair your device."}`, http.StatusUnauthorized)
		return
	}

	if err := h.manager.HandleWebSocket(w, r, subdomain); err != nil {
		log.Printf("[tunnel] WebSocket upgrade failed for %s: %v", subdomain, err)
		return
	}
}

func (h *Handler) HandleEventConnect(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	parts := strings.Split(path, "/")
	if len(parts) < 4 {
		http.Error(w, `{"error":"subdomain_required","message":"Subdomain is required"}`, http.StatusBadRequest)
		return
	}
	subdomain := parts[3]

	device, err := h.db.GetDeviceBySubdomain(subdomain)
	if err != nil {
		log.Printf("[events] Device not found: %s", subdomain)
		http.Error(w, `{"error":"device_not_found","message":"Device not found. Please re-pair your device."}`, http.StatusNotFound)
		return
	}

	authUser := r.URL.Query().Get("auth_user")
	authPassword := r.URL.Query().Get("auth_password")

	if authUser != device.AuthUser || authPassword != device.AuthPassword {
		log.Printf("[events] Auth failed for device: %s", subdomain)
		http.Error(w, `{"error":"auth_failed","message":"Authentication failed. Please re-pair your device."}`, http.StatusUnauthorized)
		return
	}

	log.Printf("[events] iOS client connecting for subdomain: %s", subdomain)
	if err := h.manager.HandleEventWebSocket(w, r, subdomain); err != nil {
		log.Printf("[events] WebSocket upgrade failed for %s: %v", subdomain, err)
		return
	}
	log.Printf("[events] iOS client connected successfully for subdomain: %s", subdomain)
}

func (h *Handler) HandleProxy(w http.ResponseWriter, r *http.Request) {
	if !h.legacyProxy {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		w.Write([]byte(`{"error":"legacy_proxy_disabled","message":"/proxy/ is disabled; use /relay/{hostId}/<path> instead"}`))
		return
	}

	path := r.URL.Path
	parts := strings.SplitN(strings.TrimPrefix(path, "/proxy/"), "/", 2)
	if len(parts) < 1 || parts[0] == "" {
		http.Error(w, "subdomain required", http.StatusBadRequest)
		return
	}

	subdomain := parts[0]
	targetPath := "/"
	if len(parts) > 1 {
		targetPath = "/" + parts[1]
	}

	log.Printf("[proxy] Incoming request: %s %s -> subdomain=%s, targetPath=%s", r.Method, path, subdomain, targetPath)

	forwardHTTP(w, r, h.manager, subdomain, targetPath)
}

// forwardHTTP sends r to hostID's tunnel at targetPath and writes the tunnel
// response back. Relay credentials and client-session headers (Authorization,
// Cookie, X-Forwarded-*, X-Real-IP) are never forwarded to the host's local
// OpenCode server.
func forwardHTTP(w http.ResponseWriter, r *http.Request, manager *Manager, hostID, targetPath string) {
	if !manager.IsConnected(hostID) {
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

	tunnelReq := buildTunnelRequest(r, targetPath, body)

	log.Printf("[forward] Forwarding request ID=%s to tunnel", tunnelReq.ID)

	resp, err := manager.ForwardRequest(hostID, tunnelReq)
	if err != nil {
		log.Printf("[forward] ForwardRequest failed: %v", err)
		if err == ErrTunnelNotFound {
			http.Error(w, "device not connected", http.StatusServiceUnavailable)
		} else if err == ErrTunnelTimeout {
			http.Error(w, "request timeout", http.StatusGatewayTimeout)
		} else {
			http.Error(w, "tunnel error", http.StatusBadGateway)
		}
		return
	}

	log.Printf("[forward] Got response: StatusCode=%d, BodyLen=%d, Headers=%v", resp.StatusCode, len(resp.Body), resp.Headers)

	for key, value := range resp.Headers {
		w.Header().Set(key, value)
	}

	w.WriteHeader(resp.StatusCode)
	if len(resp.Body) > 0 {
		n, err := w.Write(resp.Body)
		if err != nil {
			log.Printf("[forward] Failed to write response body: %v", err)
		} else {
			log.Printf("[forward] Wrote %d bytes to response", n)
		}
	}

	log.Printf("[forward] Request completed successfully")
}

// buildTunnelRequest snapshots r into a tunnel request. Relay credentials and
// client-session headers are never forwarded to the host's local OpenCode
// server.
func buildTunnelRequest(r *http.Request, targetPath string, body []byte) *TunnelRequest {
	headers := make(map[string]string)
	for key := range r.Header {
		if isHopByHopHeader(key) || isRelayCredentialHeader(key) {
			continue
		}
		headers[key] = r.Header.Get(key)
	}

	if r.URL.RawQuery != "" {
		targetPath = targetPath + "?" + r.URL.RawQuery
	}

	return &TunnelRequest{
		ID:      GenerateRequestID(),
		Method:  r.Method,
		Path:    targetPath,
		Headers: headers,
		Body:    body,
	}
}

var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func isHopByHopHeader(header string) bool {
	return hopByHopHeaders[header]
}

// isRelayCredentialHeader reports headers that belong to the relay hop or to
// the client's own browser session and must not reach the host.
func isRelayCredentialHeader(header string) bool {
	return strings.EqualFold(header, "Authorization") ||
		strings.EqualFold(header, "Cookie") ||
		strings.EqualFold(header, "X-Real-IP") ||
		strings.HasPrefix(strings.ToLower(header), "x-forwarded-")
}

type DeviceVerifyRequest struct {
	Subdomain    string `json:"subdomain"`
	AuthUser     string `json:"auth_user"`
	AuthPassword string `json:"auth_password"`
}

func (h *Handler) HandleDeviceVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, `{"error":"method_not_allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req DeviceVerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid_request","message":"Invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	device, err := h.db.GetDeviceBySubdomain(req.Subdomain)
	if err != nil {
		log.Printf("[verify] Device not found: %s", req.Subdomain)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"valid":false,"error":"device_not_found","message":"Device not found. Please re-pair your device."}`))
		return
	}

	if req.AuthUser != device.AuthUser || req.AuthPassword != device.AuthPassword {
		log.Printf("[verify] Auth failed for device: %s", req.Subdomain)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"valid":false,"error":"auth_failed","message":"Authentication failed. Please re-pair your device."}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"valid":true}`))
}
