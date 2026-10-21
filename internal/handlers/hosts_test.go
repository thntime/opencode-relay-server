package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/zero469/opencode-relay-server/internal/config"
	"github.com/zero469/opencode-relay-server/internal/database"
	"github.com/zero469/opencode-relay-server/internal/handlers"
	"github.com/zero469/opencode-relay-server/internal/middleware"
	"github.com/zero469/opencode-relay-server/internal/models"
	"github.com/zero469/opencode-relay-server/internal/services"
)

const testJWTSecret = "test-secret"

type hostTestEnv struct {
	db        database.Database
	auth      *services.AuthService
	deviceSvc *services.DeviceService
	mux       *http.ServeMux
	user1     *models.User
	user2     *models.User
	token1    string
	token2    string
	device1   *models.Device
	device2   *models.Device
}

func newHostTestEnv(t *testing.T) *hostTestEnv {
	t.Helper()

	db, err := database.NewSQLite(":memory:")
	if err != nil {
		t.Fatalf("failed to create test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	auth := services.NewAuthService(db, testJWTSecret, false)
	deviceSvc := services.NewDeviceService(db, &config.Config{})

	hash, err := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	user1, err := db.CreateUser("user1@example.com", string(hash))
	if err != nil {
		t.Fatalf("failed to create user1: %v", err)
	}
	user2, err := db.CreateUser("user2@example.com", string(hash))
	if err != nil {
		t.Fatalf("failed to create user2: %v", err)
	}

	token1, _, err := auth.Login("user1@example.com", "password123")
	if err != nil {
		t.Fatalf("failed to login user1: %v", err)
	}
	token2, _, err := auth.Login("user2@example.com", "password123")
	if err != nil {
		t.Fatalf("failed to login user2: %v", err)
	}

	device1, err := deviceSvc.Register(user1.ID, "Host1")
	if err != nil {
		t.Fatalf("failed to register device1: %v", err)
	}
	device2, err := deviceSvc.Register(user2.ID, "Host2")
	if err != nil {
		t.Fatalf("failed to register device2: %v", err)
	}
	if err := db.SetDeviceKeyMaterial(device1.ID, "wrapped-key", "salt-value", "pbkdf2"); err != nil {
		t.Fatalf("failed to set key material: %v", err)
	}

	hostHandler := handlers.NewHostHandler(deviceSvc)
	deviceHandler := handlers.NewDeviceHandler(deviceSvc)
	authMiddleware := middleware.Auth(auth)
	mux := http.NewServeMux()
	mux.Handle("GET /api/hosts", authMiddleware(http.HandlerFunc(hostHandler.List)))
	mux.Handle("GET /api/hosts/{hostId}/key", authMiddleware(http.HandlerFunc(hostHandler.GetKey)))
	mux.Handle("PUT /api/hosts/{hostId}/key", authMiddleware(http.HandlerFunc(hostHandler.PutKey)))
	mux.Handle("POST /api/devices", authMiddleware(http.HandlerFunc(deviceHandler.Register)))

	return &hostTestEnv{
		db:        db,
		auth:      auth,
		deviceSvc: deviceSvc,
		mux:       mux,
		user1:     user1,
		user2:     user2,
		token1:    token1,
		token2:    token2,
		device1:   device1,
		device2:   device2,
	}
}

func (e *hostTestEnv) do(t *testing.T, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *hostTestEnv) doJSON(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func expiredToken(t *testing.T) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": float64(1),
		"exp":     time.Now().Add(-time.Hour).Unix(),
	})
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("failed to sign expired token: %v", err)
	}
	return signed
}

func TestHostHandler_List(t *testing.T) {
	env := newHostTestEnv(t)

	t.Run("user sees only own hosts", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/api/hosts", env.token1)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		var hosts []models.HostDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &hosts); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(hosts) != 1 {
			t.Fatalf("got %d hosts, want 1: %s", len(hosts), rec.Body.String())
		}
		if hosts[0].ID != env.device1.Subdomain || hosts[0].Name != "Host1" {
			t.Errorf("host = %+v, want id=%s name=Host1", hosts[0], env.device1.Subdomain)
		}
	})

	t.Run("second user sees only own host", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/api/hosts", env.token2)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		var hosts []models.HostDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &hosts); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(hosts) != 1 || hosts[0].ID != env.device2.Subdomain {
			t.Errorf("hosts = %+v, want only %s", hosts, env.device2.Subdomain)
		}
	})

	t.Run("empty list is an array", func(t *testing.T) {
		hash, _ := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.DefaultCost)
		if _, err := env.db.CreateUser("empty@example.com", string(hash)); err != nil {
			t.Fatalf("failed to create user: %v", err)
		}
		token, _, err := env.auth.Login("empty@example.com", "password123")
		if err != nil {
			t.Fatalf("failed to login: %v", err)
		}

		rec := env.do(t, http.MethodGet, "/api/hosts", token)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Errorf("body = %q, want []", strings.TrimSpace(rec.Body.String()))
		}
	})

	t.Run("credentials are not exposed", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/api/hosts", env.token1)
		body := rec.Body.String()
		for _, secret := range []string{"auth_password", "auth_user", "key_wrapped", "key_salt", "key_kdf"} {
			if strings.Contains(body, secret) {
				t.Errorf("response leaks %q: %s", secret, body)
			}
		}
	})

	t.Run("unauthenticated is 401", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/api/hosts", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("invalid token is 401", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/api/hosts", "not-a-token")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("expired token is 401", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/api/hosts", expiredToken(t))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
}

func TestHostHandler_GetKey(t *testing.T) {
	env := newHostTestEnv(t)

	t.Run("owner gets key material", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/api/hosts/"+env.device1.Subdomain+"/key", env.token1)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}

		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		want := map[string]string{
			"key_wrapped": "wrapped-key",
			"key_salt":    "salt-value",
			"key_kdf":     "pbkdf2",
		}
		for k, v := range want {
			if body[k] != v {
				t.Errorf("%s = %q, want %q", k, body[k], v)
			}
		}
	})

	t.Run("other user is 403", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/api/hosts/"+env.device1.Subdomain+"/key", env.token2)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown host is 404", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/api/hosts/does-not-exist/key", env.token1)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unauthenticated is 401", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/api/hosts/"+env.device1.Subdomain+"/key", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("expired token is 401", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/api/hosts/"+env.device1.Subdomain+"/key", expiredToken(t))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
}

func TestHostHandler_PutKey(t *testing.T) {
	env := newHostTestEnv(t)

	t.Run("owner uploads and reads back", func(t *testing.T) {
		body := `{"key_wrapped":"new-wrapped","key_salt":"new-salt","key_kdf":"argon2id"}`
		rec := env.doJSON(t, http.MethodPut, "/api/hosts/"+env.device2.Subdomain+"/key", env.token2, body)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}
		if rec.Body.Len() != 0 {
			t.Errorf("body = %q, want empty", rec.Body.String())
		}

		rec = env.do(t, http.MethodGet, "/api/hosts/"+env.device2.Subdomain+"/key", env.token2)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		var got map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		want := map[string]string{
			"key_wrapped": "new-wrapped",
			"key_salt":    "new-salt",
			"key_kdf":     "argon2id",
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %q, want %q", k, got[k], v)
			}
		}
	})

	t.Run("oversized body is rejected", func(t *testing.T) {
		big := `{"key_wrapped":"` + strings.Repeat("a", 1<<20) + `"}`
		rec := env.doJSON(t, http.MethodPut, "/api/hosts/"+env.device2.Subdomain+"/key", env.token2, big)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 400/413; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing key_wrapped is 400", func(t *testing.T) {
		rec := env.doJSON(t, http.MethodPut, "/api/hosts/"+env.device2.Subdomain+"/key", env.token2, `{"key_salt":"s","key_kdf":"argon2id"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
		var body models.ErrorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if body.Error != "key_wrapped is required" {
			t.Errorf("error = %q, want %q", body.Error, "key_wrapped is required")
		}
	})

	t.Run("other user is 403", func(t *testing.T) {
		rec := env.doJSON(t, http.MethodPut, "/api/hosts/"+env.device1.Subdomain+"/key", env.token2, `{"key_wrapped":"x"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown host is 404", func(t *testing.T) {
		rec := env.doJSON(t, http.MethodPut, "/api/hosts/does-not-exist/key", env.token1, `{"key_wrapped":"x"}`)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unauthenticated is 401", func(t *testing.T) {
		rec := env.doJSON(t, http.MethodPut, "/api/hosts/"+env.device1.Subdomain+"/key", "", `{"key_wrapped":"x"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
}

func TestHostHandler_ListOnlineState(t *testing.T) {
	env := newHostTestEnv(t)

	if err := env.db.UpdateDeviceHeartbeat(env.device1.ID); err != nil {
		t.Fatalf("failed to update heartbeat: %v", err)
	}

	rec := env.do(t, http.MethodGet, "/api/hosts", env.token1)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var hosts []models.HostDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &hosts); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("got %d hosts, want 1: %s", len(hosts), rec.Body.String())
	}
	if !hosts[0].Online {
		t.Errorf("online = false, want true")
	}
	if hosts[0].LastSeen.IsZero() {
		t.Errorf("last_seen is zero, want non-zero")
	}

	if err := env.db.MarkDeviceOffline(env.device1.ID); err != nil {
		t.Fatalf("failed to mark device offline: %v", err)
	}

	rec = env.do(t, http.MethodGet, "/api/hosts", env.token1)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	hosts = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &hosts); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(hosts) != 1 || hosts[0].ID != env.device1.Subdomain {
		t.Fatalf("hosts = %+v, want only %s", hosts, env.device1.Subdomain)
	}
	if hosts[0].Online {
		t.Errorf("online = true, want false")
	}
}
