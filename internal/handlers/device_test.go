package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/zero469/opencode-relay-server/internal/config"
	"github.com/zero469/opencode-relay-server/internal/handlers"
	"github.com/zero469/opencode-relay-server/internal/middleware"
	"github.com/zero469/opencode-relay-server/internal/models"
	"github.com/zero469/opencode-relay-server/internal/services"
)

func TestDeviceHandler_RegisterDefaultName(t *testing.T) {
	env := newHostTestEnv(t)

	t.Run("empty name gets default", func(t *testing.T) {
		rec := env.doJSON(t, http.MethodPost, "/api/devices", env.token1, `{}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
		}
		var device models.Device
		if err := json.Unmarshal(rec.Body.Bytes(), &device); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if device.Name != "OpenCode Host" {
			t.Errorf("name = %q, want %q", device.Name, "OpenCode Host")
		}
	})

	t.Run("explicit name is kept", func(t *testing.T) {
		rec := env.doJSON(t, http.MethodPost, "/api/devices", env.token1, `{"name":"My Laptop"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
		}
		var device models.Device
		if err := json.Unmarshal(rec.Body.Bytes(), &device); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if device.Name != "My Laptop" {
			t.Errorf("name = %q, want %q", device.Name, "My Laptop")
		}
	})
}

func newFrpcConfigMux(t *testing.T, deviceSvc *services.DeviceService, auth *services.AuthService) *http.ServeMux {
	t.Helper()
	h := handlers.NewDeviceHandler(deviceSvc)
	mux := http.NewServeMux()
	mux.Handle("GET /api/devices/{id}/frpc-config", middleware.Auth(auth)(http.HandlerFunc(h.GetFrpcConfig)))
	return mux
}

func getFrpcConfig(t *testing.T, mux *http.ServeMux, token string, deviceID int64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/devices/"+strconv.FormatInt(deviceID, 10)+"/frpc-config", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestDeviceHandler_GetFrpcConfig_MissingFrpsConfig(t *testing.T) {
	env := newHostTestEnv(t)
	deviceSvc := services.NewDeviceService(env.db, &config.Config{FrpsToken: "secret-token"})
	mux := newFrpcConfigMux(t, deviceSvc, env.auth)

	rec := getFrpcConfig(t, mux, env.token1, env.device1.ID)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}

	var body models.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.Error == "" {
		t.Fatal("error message is empty, want readable message naming missing variables")
	}
	for _, name := range []string{"FRPS_HOST", "FRPS_PORT", "FRPS_DOMAIN"} {
		if !strings.Contains(body.Error, name) {
			t.Errorf("error = %q, want it to name %s", body.Error, name)
		}
	}
	if strings.Contains(rec.Body.String(), "secret-token") {
		t.Errorf("response leaked FRPS_TOKEN: %s", rec.Body.String())
	}
}

// 配置校验不能先于设备归属/存在性检查：设备不存在时必须返回 404，
// 否则会向未授权调用方泄露服务端配置状态。
func TestDeviceHandler_GetFrpcConfig_MissingConfigAndDeviceNotFound(t *testing.T) {
	env := newHostTestEnv(t)
	deviceSvc := services.NewDeviceService(env.db, &config.Config{})
	mux := newFrpcConfigMux(t, deviceSvc, env.auth)

	rec := getFrpcConfig(t, mux, env.token1, 999999)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

func TestDeviceHandler_GetFrpcConfig_DeviceNotFound(t *testing.T) {
	env := newHostTestEnv(t)
	deviceSvc := services.NewDeviceService(env.db, &config.Config{
		FrpsHost: "frps.example.com",
		FrpsPort: "7000",
		Domain:   "example.com",
	})
	mux := newFrpcConfigMux(t, deviceSvc, env.auth)

	rec := getFrpcConfig(t, mux, env.token1, 999999)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}

	var body models.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.Error != "device not found" {
		t.Errorf("error = %q, want %q", body.Error, "device not found")
	}
}
