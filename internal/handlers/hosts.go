package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/zero469/opencode-relay-server/internal/middleware"
	"github.com/zero469/opencode-relay-server/internal/models"
	"github.com/zero469/opencode-relay-server/internal/services"
)

type HostHandler struct {
	deviceService *services.DeviceService
}

func NewHostHandler(deviceService *services.DeviceService) *HostHandler {
	return &HostHandler{deviceService: deviceService}
}

func (h *HostHandler) List(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetUserID(r)

	devices, err := h.deviceService.List(userID)
	if err != nil {
		writeError(w, "failed to list hosts", http.StatusInternalServerError)
		return
	}

	hosts := make([]models.HostDTO, 0, len(devices))
	for _, device := range devices {
		hosts = append(hosts, models.HostDTO{
			ID:       device.Subdomain,
			Name:     device.Name,
			Online:   device.Online,
			LastSeen: device.LastSeen,
		})
	}

	writeJSON(w, hosts, http.StatusOK)
}

func (h *HostHandler) GetKey(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetUserID(r)

	device, err := h.deviceService.ResolveHost(userID, r.PathValue("hostId"))
	if err != nil {
		switch {
		case errors.Is(err, services.ErrHostForbidden):
			writeError(w, "host forbidden", http.StatusForbidden)
		case errors.Is(err, services.ErrHostNotFound):
			writeError(w, "host not found", http.StatusNotFound)
		default:
			writeError(w, "failed to get host key", http.StatusInternalServerError)
		}
		return
	}

	keyWrapped, keySalt, keyKdf, err := h.deviceService.GetKeyMaterial(device.ID)
	if err != nil {
		writeError(w, "failed to get host key", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]string{
		"key_wrapped": keyWrapped,
		"key_salt":    keySalt,
		"key_kdf":     keyKdf,
	}, http.StatusOK)
}

func (h *HostHandler) PutKey(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetUserID(r)
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req struct {
		KeyWrapped string `json:"key_wrapped"`
		KeySalt    string `json:"key_salt"`
		KeyKdf     string `json:"key_kdf"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.KeyWrapped == "" {
		writeError(w, "key_wrapped is required", http.StatusBadRequest)
		return
	}

	device, err := h.deviceService.ResolveHost(userID, r.PathValue("hostId"))
	if err != nil {
		switch {
		case errors.Is(err, services.ErrHostForbidden):
			writeError(w, "host forbidden", http.StatusForbidden)
		case errors.Is(err, services.ErrHostNotFound):
			writeError(w, "host not found", http.StatusNotFound)
		default:
			writeError(w, "failed to update host key", http.StatusInternalServerError)
		}
		return
	}

	if err := h.deviceService.SetKeyMaterial(device.ID, req.KeyWrapped, req.KeySalt, req.KeyKdf); err != nil {
		writeError(w, "failed to update host key", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
