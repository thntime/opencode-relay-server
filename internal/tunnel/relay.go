package tunnel

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/zero469/opencode-relay-server/internal/middleware"
	"github.com/zero469/opencode-relay-server/internal/models"
	"github.com/zero469/opencode-relay-server/internal/services"
)

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
	forwardHTTP(w, r, h.manager, device.Subdomain, targetPath)
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
