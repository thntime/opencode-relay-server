package tunnel_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"

	"github.com/zero469/opencode-relay-server/internal/config"
	"github.com/zero469/opencode-relay-server/internal/database"
	"github.com/zero469/opencode-relay-server/internal/middleware"
	"github.com/zero469/opencode-relay-server/internal/models"
	"github.com/zero469/opencode-relay-server/internal/services"
	"github.com/zero469/opencode-relay-server/internal/tunnel"
)

const relayTestJWTSecret = "relay-test-secret"

type relayTestEnv struct {
	db      database.Database
	auth    *services.AuthService
	devices *services.DeviceService
	manager *tunnel.Manager
	mux     *http.ServeMux
	user1   *models.User
	user2   *models.User
	token1  string
	token2  string
	device1 *models.Device
	device2 *models.Device
}

func newRelayTestEnv(t *testing.T) *relayTestEnv {
	t.Helper()

	db, err := database.NewSQLite(":memory:")
	if err != nil {
		t.Fatalf("failed to create test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	auth := services.NewAuthService(db, relayTestJWTSecret, false)
	deviceSvc := services.NewDeviceService(db, &config.Config{})

	hash, err := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}
	user1, err := db.CreateUser("relay1@example.com", string(hash))
	if err != nil {
		t.Fatalf("failed to create user1: %v", err)
	}
	user2, err := db.CreateUser("relay2@example.com", string(hash))
	if err != nil {
		t.Fatalf("failed to create user2: %v", err)
	}
	token1, _, err := auth.Login("relay1@example.com", "password123")
	if err != nil {
		t.Fatalf("failed to login user1: %v", err)
	}
	token2, _, err := auth.Login("relay2@example.com", "password123")
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

	manager := tunnel.NewManager()
	relay := tunnel.NewRelayHandler(manager, deviceSvc)
	legacy := tunnel.NewHandler(manager, db, false)

	authMiddleware := middleware.Auth(auth)
	mux := http.NewServeMux()
	mux.Handle("/relay/", authMiddleware(http.HandlerFunc(relay.HandleRelay)))
	mux.Handle("/proxy/", http.HandlerFunc(legacy.HandleProxy))

	return &relayTestEnv{
		db:      db,
		auth:    auth,
		devices: deviceSvc,
		manager: manager,
		mux:     mux,
		user1:   user1,
		user2:   user2,
		token1:  token1,
		token2:  token2,
		device1: device1,
		device2: device2,
	}
}

func (e *relayTestEnv) do(t *testing.T, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// connectFakeTunnel registers a fake tunnel client for subdomain. reply, when
// non-nil, is called for every forwarded request and its result written back.
func connectFakeTunnel(t *testing.T, manager *tunnel.Manager, subdomain string, reply func(*tunnel.TunnelRequest) *tunnel.TunnelResponse, proto ...string) *websocket.Conn {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		manager.HandleWebSocket(w, r, subdomain)
	}))
	header := http.Header{}
	if len(proto) > 0 {
		header.Set("X-Tunnel-Protocol", proto[0])
	}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/tunnel", header)
	if err != nil {
		srv.Close()
		t.Fatalf("failed to dial fake tunnel: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		srv.Close()
	})

	if reply != nil {
		go func() {
			for {
				var req tunnel.TunnelRequest
				if err := conn.ReadJSON(&req); err != nil {
					return
				}
				if resp := reply(&req); resp != nil {
					conn.WriteJSON(resp)
				}
			}
		}()
	}

	deadline := time.Now().Add(3 * time.Second)
	for !manager.IsConnected(subdomain) {
		if time.Now().After(deadline) {
			t.Fatalf("fake tunnel for %s did not connect", subdomain)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return conn
}

func TestLegacyProxySwitch(t *testing.T) {
	env := newRelayTestEnv(t)

	t.Run("disabled by default", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/proxy/"+env.device1.Subdomain+"/v1/models", "")
		if rec.Code != http.StatusGone {
			t.Fatalf("status = %d, want 410; body = %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "/relay/") {
			t.Errorf("body = %q, want migration hint mentioning /relay/", rec.Body.String())
		}
	})

	t.Run("enabled keeps old proxy behavior", func(t *testing.T) {
		legacy := tunnel.NewHandler(env.manager, env.db, true)
		mux := http.NewServeMux()
		mux.Handle("/proxy/", http.HandlerFunc(legacy.HandleProxy))

		req := httptest.NewRequest(http.MethodGet, "/proxy/"+env.device1.Subdomain+"/v1/models", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 from old proxy path; body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestRelayHandler_Forward(t *testing.T) {
	env := newRelayTestEnv(t)

	t.Run("unauthenticated is 401", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/relay/"+env.device1.Subdomain+"/v1/models", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("other user's host is 403", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/relay/"+env.device1.Subdomain+"/v1/models", env.token2)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown host is 404", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/relay/does-not-exist/v1/models", env.token1)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("offline host is 503", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/relay/"+env.device1.Subdomain+"/v1/models", env.token1)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("forwards path, query and body without credentials or forwarded headers", func(t *testing.T) {
		captured := make(chan *tunnel.TunnelRequest, 1)
		connectFakeTunnel(t, env.manager, env.device1.Subdomain, func(req *tunnel.TunnelRequest) *tunnel.TunnelResponse {
			captured <- req
			return &tunnel.TunnelResponse{
				ID:         req.ID,
				StatusCode: http.StatusOK,
				Headers:    map[string]string{"Content-Type": "application/json"},
				Body:       []byte(`{"ok":true}`),
			}
		})

		req := httptest.NewRequest(http.MethodGet, "/relay/"+env.device1.Subdomain+"/v1/models?limit=5", nil)
		req.Header.Set("Authorization", "Bearer "+env.token1)
		req.Header.Set("Cookie", "session=secret")
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Real-IP", "203.0.113.7")
		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if rec.Body.String() != `{"ok":true}` {
			t.Errorf("body = %q, want forwarded body", rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}

		select {
		case req := <-captured:
			if req.Method != http.MethodGet {
				t.Errorf("forwarded method = %q, want GET", req.Method)
			}
			if req.Path != "/v1/models?limit=5" {
				t.Errorf("forwarded path = %q, want /v1/models?limit=5", req.Path)
			}
			for key := range req.Headers {
				switch {
				case strings.EqualFold(key, "Authorization"),
					strings.EqualFold(key, "Cookie"),
					strings.EqualFold(key, "X-Real-IP"),
					strings.HasPrefix(strings.ToLower(key), "x-forwarded-"):
					t.Errorf("%s must not be forwarded, headers = %v", key, req.Headers)
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatal("request was not forwarded to the tunnel")
		}
	})
}

func TestRelayHandler_PathParsing(t *testing.T) {
	env := newRelayTestEnv(t)

	t.Run("missing host is 400", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/relay/", env.token1)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("host without trailing path forwards to root", func(t *testing.T) {
		captured := make(chan *tunnel.TunnelRequest, 1)
		connectFakeTunnel(t, env.manager, env.device1.Subdomain, func(req *tunnel.TunnelRequest) *tunnel.TunnelResponse {
			captured <- req
			return &tunnel.TunnelResponse{ID: req.ID, StatusCode: http.StatusOK}
		})

		rec := env.do(t, http.MethodGet, "/relay/"+env.device1.Subdomain, env.token1)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		select {
		case req := <-captured:
			if req.Path != "/" {
				t.Errorf("forwarded path = %q, want /", req.Path)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("request was not forwarded to the tunnel")
		}
	})

	t.Run("encoded slash still goes through ownership check", func(t *testing.T) {
		rec := env.do(t, http.MethodGet, "/relay/does-not-exist%2Fextra", env.token1)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})
}
