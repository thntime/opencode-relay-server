package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/zero469/opencode-relay-server/internal/middleware"
	"github.com/zero469/opencode-relay-server/internal/models"
	"github.com/zero469/opencode-relay-server/internal/services"
)

type DeviceHandler struct {
	deviceService *services.DeviceService
}

func NewDeviceHandler(deviceService *services.DeviceService) *DeviceHandler {
	return &DeviceHandler{deviceService: deviceService}
}

func (h *DeviceHandler) Register(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetUserID(r)

	var req models.DeviceRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		req.Name = "OpenCode Host"
	}

	device, err := h.deviceService.Register(userID, req.Name)
	if err != nil {
		writeError(w, "failed to register device", http.StatusInternalServerError)
		return
	}

	writeJSON(w, device, http.StatusCreated)
}

func (h *DeviceHandler) List(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetUserID(r)

	devices, err := h.deviceService.List(userID)
	if err != nil {
		writeError(w, "failed to list devices", http.StatusInternalServerError)
		return
	}

	if devices == nil {
		devices = []*models.Device{}
	}

	writeJSON(w, devices, http.StatusOK)
}

func (h *DeviceHandler) Get(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetUserID(r)
	deviceID := getDeviceID(r)

	device, err := h.deviceService.Get(userID, deviceID)
	if err != nil {
		writeError(w, "device not found", http.StatusNotFound)
		return
	}

	writeJSON(w, device, http.StatusOK)
}

func (h *DeviceHandler) Update(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetUserID(r)
	deviceID := getDeviceID(r)

	var req models.DeviceUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		writeError(w, "device name is required", http.StatusBadRequest)
		return
	}

	device, err := h.deviceService.Update(userID, deviceID, req.Name)
	if err != nil {
		writeError(w, "device not found", http.StatusNotFound)
		return
	}

	writeJSON(w, device, http.StatusOK)
}

func (h *DeviceHandler) Delete(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetUserID(r)
	deviceID := getDeviceID(r)

	if err := h.deviceService.Delete(userID, deviceID); err != nil {
		writeError(w, "device not found", http.StatusNotFound)
		return
	}

	writeJSON(w, models.MessageResponse{Message: "device deleted"}, http.StatusOK)
}

func (h *DeviceHandler) Heartbeat(w http.ResponseWriter, r *http.Request) {
	subdomain := r.URL.Query().Get("subdomain")
	if subdomain == "" {
		writeError(w, "subdomain is required", http.StatusBadRequest)
		return
	}

	if err := h.deviceService.Heartbeat(subdomain); err != nil {
		writeError(w, "device not found", http.StatusNotFound)
		return
	}

	writeJSON(w, models.MessageResponse{Message: "ok"}, http.StatusOK)
}

func (h *DeviceHandler) GetFrpcConfig(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetUserID(r)
	deviceID := getDeviceID(r)
	localPort := r.URL.Query().Get("local_port")
	if localPort == "" {
		localPort = "4096"
	}

	config, err := h.deviceService.GetFrpcConfig(userID, deviceID, localPort)
	if err != nil {
		writeError(w, "device not found", http.StatusNotFound)
		return
	}

	writeJSON(w, config, http.StatusOK)
}

func getDeviceID(r *http.Request) int64 {
	path := r.URL.Path
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part == "devices" && i+1 < len(parts) {
			id, _ := strconv.ParseInt(parts[i+1], 10, 64)
			return id
		}
	}
	return 0
}
