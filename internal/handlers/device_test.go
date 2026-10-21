package handlers_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zero469/opencode-relay-server/internal/models"
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
