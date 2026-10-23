package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zero469/opencode-relay-server/internal/tunnel"
)

// withTempConfigDir isolates all config reads/writes from the real HOME.
func withTempConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := configDirOverride
	configDirOverride = dir
	t.Cleanup(func() { configDirOverride = old })
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadDeviceConfigLegacyFillsHostID(t *testing.T) {
	dir := withTempConfigDir(t)
	writeFile(t, filepath.Join(dir, deviceFileName), `{
  "id": 7,
  "name": "old-host",
  "relay_url": "http://relay",
  "subdomain": "abc123",
  "auth_user": "u",
  "auth_password": "p"
}`)

	device, err := loadDeviceConfig()
	if err != nil {
		t.Fatalf("loadDeviceConfig: %v", err)
	}
	if device.HostID != device.Subdomain {
		t.Fatalf("HostID = %q, want %q", device.HostID, device.Subdomain)
	}
	if device.HostName == "" {
		t.Fatal("HostName should default to os.Hostname()")
	}
}

func TestLoadExistingDevice(t *testing.T) {
	withTempConfigDir(t)

	if device := loadExistingDevice(false); device != nil {
		t.Fatalf("expected nil without device.json, got %+v", device)
	}

	if err := saveDeviceConfig(&DeviceConfig{
		DeviceName:   "box",
		RelayURL:     "http://relay",
		Subdomain:    "s1",
		HostID:       "s1",
		AuthUser:     "u",
		AuthPassword: "p",
	}); err != nil {
		t.Fatalf("saveDeviceConfig: %v", err)
	}

	if device := loadExistingDevice(false); device == nil || device.HostID != "s1" {
		t.Fatalf("expected saved identity, got %+v", device)
	}
	if device := loadExistingDevice(true); device != nil {
		t.Fatalf("--reset must ignore saved identity, got %+v", device)
	}
}

func TestParseStartArgs(t *testing.T) {
	opts := parseStartArgs([]string{
		"start",
		"-port", "5000",
		"-relay", "http://relay",
		"--name", "box",
		"--token", "tok",
		"--email", "e@x",
		"--password", "pw",
		"--passphrase", "pp",
		"--rewrap",
		"--reset",
		"--attach",
	})

	if opts.Port != "5000" || opts.Relay != "http://relay" {
		t.Fatalf("legacy flags not parsed: %+v", opts)
	}
	if opts.Name != "box" || opts.Token != "tok" || opts.Email != "e@x" || opts.Password != "pw" || opts.Passphrase != "pp" {
		t.Fatalf("new value flags not parsed: %+v", opts)
	}
	if !opts.Rewrap || !opts.Reset || !opts.Attach {
		t.Fatalf("new bool flags not parsed: %+v", opts)
	}

	defaults := parseStartArgs([]string{"start"})
	if defaults.Relay != defaultRelay || defaults.Port != "" {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}
}

func TestResolveAuthFlagTokenWins(t *testing.T) {
	withTempConfigDir(t)
	if err := saveAuthConfig(&AuthConfig{RelayURL: "http://relay", Token: "from-file"}); err != nil {
		t.Fatalf("saveAuthConfig: %v", err)
	}
	t.Setenv("OPENCODE_RELAY_TOKEN", "from-env")

	auth, err := resolveAuth("http://relay", startOptions{Token: "from-flag"})
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if auth.Token != "from-flag" {
		t.Fatalf("token = %q, want from-flag", auth.Token)
	}
}

func TestResolveAuthEnvToken(t *testing.T) {
	withTempConfigDir(t)
	if err := saveAuthConfig(&AuthConfig{RelayURL: "http://relay", Token: "from-file"}); err != nil {
		t.Fatalf("saveAuthConfig: %v", err)
	}
	t.Setenv("OPENCODE_RELAY_TOKEN", "from-env")

	auth, err := resolveAuth("http://relay", startOptions{})
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if auth.Token != "from-env" {
		t.Fatalf("token = %q, want from-env", auth.Token)
	}
}

func TestResolveAuthSavedToken(t *testing.T) {
	withTempConfigDir(t)
	t.Setenv("OPENCODE_RELAY_TOKEN", "")
	if err := saveAuthConfig(&AuthConfig{RelayURL: "http://saved", Token: "from-file", Email: "e@x"}); err != nil {
		t.Fatalf("saveAuthConfig: %v", err)
	}

	auth, err := resolveAuth("http://relay", startOptions{})
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if auth.Token != "from-file" {
		t.Fatalf("token = %q, want from-file", auth.Token)
	}
}

func TestResolveAuthEmailLogin(t *testing.T) {
	dir := withTempConfigDir(t)
	t.Setenv("OPENCODE_RELAY_TOKEN", "")

	var logins int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/login" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&logins, 1)
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["email"] != "e@x" || body["password"] != "pw" {
			t.Errorf("unexpected login body: %v", body)
		}
		w.Write([]byte(`{"token":"tok-login","user":{"id":1,"email":"e@x"}}`))
	}))
	defer srv.Close()

	auth, err := resolveAuth(srv.URL, startOptions{Email: "e@x", Password: "pw"})
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if auth.Token != "tok-login" {
		t.Fatalf("token = %q, want tok-login", auth.Token)
	}
	if logins != 1 {
		t.Fatalf("login calls = %d, want 1", logins)
	}

	saved, err := loadAuthConfig()
	if err != nil {
		t.Fatalf("login token not persisted: %v", err)
	}
	if saved.Token != "tok-login" {
		t.Fatalf("saved token = %q, want tok-login", saved.Token)
	}

	info, err := os.Stat(filepath.Join(dir, authFileName))
	if err != nil {
		t.Fatalf("stat auth.json: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("auth.json mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestRegisterHostReusesExisting(t *testing.T) {
	var posts, gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token: %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/devices":
			atomic.AddInt32(&gets, 1)
			w.Write([]byte(`[
  {"id":5,"name":"other","subdomain":"other1","auth_user":"ou","auth_password":"op"},
  {"id":9,"name":"box","subdomain":"s1","auth_user":"u1","auth_password":"p1"}
]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/devices":
			atomic.AddInt32(&posts, 1)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":10,"name":"box","subdomain":"new","auth_user":"nu","auth_password":"np"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	device, err := registerHost(srv.URL, "tok", "box")
	if err != nil {
		t.Fatalf("registerHost: %v", err)
	}
	if posts != 0 {
		t.Fatalf("POST /api/devices called %d times, want 0 on name match", posts)
	}
	if gets != 1 {
		t.Fatalf("GET /api/devices calls = %d, want 1", gets)
	}
	if device.HostID != "s1" || device.Subdomain != "s1" || device.AuthUser != "u1" || device.AuthPassword != "p1" {
		t.Fatalf("unexpected reused device: %+v", device)
	}
	if device.DeviceID != 9 || device.DeviceName != "box" {
		t.Fatalf("unexpected reused identity: %+v", device)
	}
}

func TestRegisterHostCreatesNew(t *testing.T) {
	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/devices":
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/devices":
			atomic.AddInt32(&posts, 1)
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "box" {
				t.Errorf("POST name = %q, want box", body["name"])
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":11,"name":"box","subdomain":"new1","auth_user":"nu","auth_password":"np"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	device, err := registerHost(srv.URL, "tok", "box")
	if err != nil {
		t.Fatalf("registerHost: %v", err)
	}
	if posts != 1 {
		t.Fatalf("POST /api/devices calls = %d, want 1", posts)
	}
	if device.HostID != "new1" || device.Subdomain != "new1" || device.AuthUser != "nu" || device.AuthPassword != "np" {
		t.Fatalf("unexpected created device: %+v", device)
	}
}

func TestEnsureHostResetConflict(t *testing.T) {
	dir := withTempConfigDir(t)
	if err := saveDeviceConfig(&DeviceConfig{
		DeviceName:   "box",
		RelayURL:     "http://relay",
		Subdomain:    "s1",
		HostID:       "s1",
		AuthUser:     "u",
		AuthPassword: "p",
	}); err != nil {
		t.Fatalf("saveDeviceConfig: %v", err)
	}

	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/devices":
			w.Write([]byte(`[{"id":9,"name":"box","subdomain":"s1","auth_user":"u1","auth_password":"p1"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/devices":
			atomic.AddInt32(&posts, 1)
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	_, err := ensureHost(srv.URL, "tok", "box", true)
	if err == nil || !strings.Contains(err.Error(), "同名主机已存在") {
		t.Fatalf("err = %v, want 同名主机已存在", err)
	}
	if posts != 0 {
		t.Fatalf("POST /api/devices calls = %d, want 0 on conflict", posts)
	}
	if _, statErr := os.Stat(filepath.Join(dir, deviceFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("device.json should be removed by --reset, stat err = %v", statErr)
	}
}

func TestEnsureHostResetRegistersNew(t *testing.T) {
	dir := withTempConfigDir(t)
	if err := saveDeviceConfig(&DeviceConfig{
		DeviceName:   "old",
		RelayURL:     "http://relay",
		Subdomain:    "old1",
		HostID:       "old1",
		AuthUser:     "u",
		AuthPassword: "p",
	}); err != nil {
		t.Fatalf("saveDeviceConfig: %v", err)
	}

	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/devices":
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/devices":
			atomic.AddInt32(&posts, 1)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":12,"name":"box","subdomain":"new2","auth_user":"nu","auth_password":"np"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	device, err := ensureHost(srv.URL, "tok", "box", true)
	if err != nil {
		t.Fatalf("ensureHost: %v", err)
	}
	if posts != 1 {
		t.Fatalf("POST /api/devices calls = %d, want 1", posts)
	}
	if device.RelayURL != srv.URL {
		t.Fatalf("RelayURL = %q, want %q", device.RelayURL, srv.URL)
	}

	saved, err := loadDeviceConfig()
	if err != nil {
		t.Fatalf("device.json not saved: %v", err)
	}
	if saved.HostID != "new2" {
		t.Fatalf("saved HostID = %q, want new2", saved.HostID)
	}

	info, err := os.Stat(filepath.Join(dir, deviceFileName))
	if err != nil {
		t.Fatalf("stat device.json: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("device.json mode = %v, want 0600", info.Mode().Perm())
	}
}

// --- 第 3 组：失败分类与身份保留 ---

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want failureKind
	}{
		{"timeout", errors.New("dial tcp 10.0.0.1:443: i/o timeout"), failureNetwork},
		{"eof", fmt.Errorf("read error: %w", io.EOF), failureNetwork},
		{"close 1006", fmt.Errorf("read error: %w", &websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: "abnormal closure"}), failureNetwork},
		{"close 1000", fmt.Errorf("read error: %w", &websocket.CloseError{Code: websocket.CloseNormalClosure}), failureNetwork},
		{"close 1001", fmt.Errorf("read error: %w", &websocket.CloseError{Code: websocket.CloseGoingAway}), failureNetwork},
		{"close 1003", fmt.Errorf("read error: %w", &websocket.CloseError{Code: websocket.CloseUnsupportedData}), failurePolicy},
		{"close 1008", fmt.Errorf("read error: %w", &websocket.CloseError{Code: websocket.ClosePolicyViolation, Text: "denied"}), failurePolicy},
		{"close 1011", fmt.Errorf("read error: %w", &websocket.CloseError{Code: websocket.CloseInternalServerErr}), failureNetwork},
		{"500", &httpStatusError{Op: "verify", Status: http.StatusInternalServerError}, failureNetwork},
		{"401", &httpStatusError{Op: "verify", Status: http.StatusUnauthorized}, failureCredential},
		{"403", &httpStatusError{Op: "dial", Status: http.StatusForbidden}, failureCredential},
		{"404", &httpStatusError{Op: "dial", Status: http.StatusNotFound}, failureNotFound},
		{"bad handshake", errors.New("dial failed: websocket: bad handshake"), failureProtocol},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyFailure(tt.err); got != tt.want {
				t.Fatalf("classifyFailure(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func saveTestDevice(t *testing.T, relay string) {
	t.Helper()
	if err := saveDeviceConfig(&DeviceConfig{
		DeviceName:   "box",
		RelayURL:     relay,
		Subdomain:    "s1",
		HostID:       "s1",
		AuthUser:     "u",
		AuthPassword: "p",
	}); err != nil {
		t.Fatalf("saveDeviceConfig: %v", err)
	}
}

func assertDeviceFileExists(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(getConfigDir(), deviceFileName)); err != nil {
		t.Fatalf("device.json must survive failure, stat err = %v", err)
	}
}

func TestRunTunnelKeepsIdentityOnVerifyFailure(t *testing.T) {
	tests := []struct {
		status int
		want   failureKind
	}{
		{http.StatusUnauthorized, failureCredential},
		{http.StatusForbidden, failureCredential},
		{http.StatusNotFound, failureNotFound},
		{http.StatusInternalServerError, failureNetwork},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			withTempConfigDir(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			saveTestDevice(t, srv.URL)
			device, err := loadDeviceConfig()
			if err != nil {
				t.Fatalf("loadDeviceConfig: %v", err)
			}

			err = runTunnel(device, "4096")
			if err == nil {
				t.Fatal("runTunnel must return the verify error")
			}
			if got := classifyFailure(err); got != tt.want {
				t.Fatalf("classifyFailure = %d, want %d (err: %v)", got, tt.want, err)
			}
			assertDeviceFileExists(t)
		})
	}
}

func TestRunTunnelProtocolFailureKeepsIdentity(t *testing.T) {
	withTempConfigDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/device/verify" {
			w.WriteHeader(http.StatusOK)
			return
		}
		// 400 on the tunnel upgrade: server refused the handshake, not auth.
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	saveTestDevice(t, srv.URL)
	device, err := loadDeviceConfig()
	if err != nil {
		t.Fatalf("loadDeviceConfig: %v", err)
	}

	err = runTunnel(device, "4096")
	if err == nil {
		t.Fatal("runTunnel must return the handshake error")
	}
	if got := classifyFailure(err); got != failureProtocol {
		t.Fatalf("classifyFailure = %d, want failureProtocol (err: %v)", got, err)
	}
	assertDeviceFileExists(t)
}

func TestConnectWithRetryAuthFailureKeepsIdentity(t *testing.T) {
	withTempConfigDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/device/verify" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	saveTestDevice(t, srv.URL)
	device, err := loadDeviceConfig()
	if err != nil {
		t.Fatalf("loadDeviceConfig: %v", err)
	}

	err = runTunnel(device, "4096")
	if err == nil {
		t.Fatal("runTunnel must return the credential error")
	}
	if got := classifyFailure(err); got != failureCredential {
		t.Fatalf("classifyFailure = %d, want failureCredential (err: %v)", got, err)
	}
	assertDeviceFileExists(t)
}

func TestReregisterFailureKeepsIdentity(t *testing.T) {
	withTempConfigDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	saveTestDevice(t, srv.URL)
	if _, err := ensureHost(srv.URL, "tok", "box", false); err == nil {
		t.Fatal("re-registration must fail on 401")
	}
	assertDeviceFileExists(t)
}

// --- 第 4 组：E2E 密钥包裹与上报 ---

func TestWrapHostKeyRoundTrip(t *testing.T) {
	hostKey, err := generateEncryptionKey()
	if err != nil {
		t.Fatalf("generateEncryptionKey: %v", err)
	}

	wrapped, salt, err := wrapHostKey("correct horse", hostKey)
	if err != nil {
		t.Fatalf("wrapHostKey: %v", err)
	}
	if wrapped == "" || salt == "" {
		t.Fatal("wrapHostKey must return wrapped key and salt")
	}
	if strings.Contains(wrapped, hostKey) {
		t.Fatal("wrapped material must not contain the raw host key")
	}

	got, err := unwrapHostKey("correct horse", wrapped, salt)
	if err != nil {
		t.Fatalf("unwrapHostKey: %v", err)
	}
	if got != hostKey {
		t.Fatalf("unwrapped key = %q, want %q", got, hostKey)
	}

	if _, err := unwrapHostKey("wrong horse", wrapped, salt); err == nil {
		t.Fatal("wrong passphrase must fail to unwrap")
	}
}

func TestEnsureHostKeyRewrapChangesSaltAndCiphertext(t *testing.T) {
	hostKey, err := generateEncryptionKey()
	if err != nil {
		t.Fatalf("generateEncryptionKey: %v", err)
	}
	device := &DeviceConfig{EncryptionKey: hostKey}

	changed, err := ensureHostKey(device, "first", false)
	if err != nil {
		t.Fatalf("ensureHostKey: %v", err)
	}
	if !changed {
		t.Fatal("first wrap must report a change")
	}
	if device.KeyKdf != keyKdfScrypt {
		t.Fatalf("KeyKdf = %q, want %q", device.KeyKdf, keyKdfScrypt)
	}
	salt1, wrapped1 := device.KeySalt, device.KeyWrapped

	// Same passphrase without --rewrap must not touch existing material.
	changed, err = ensureHostKey(device, "first", false)
	if err != nil {
		t.Fatalf("ensureHostKey: %v", err)
	}
	if changed || device.KeySalt != salt1 || device.KeyWrapped != wrapped1 {
		t.Fatal("existing wrapped material must be reused without --rewrap")
	}

	changed, err = ensureHostKey(device, "second", true)
	if err != nil {
		t.Fatalf("ensureHostKey --rewrap: %v", err)
	}
	if !changed {
		t.Fatal("--rewrap must report a change")
	}
	if device.KeySalt == salt1 {
		t.Fatal("--rewrap must use a fresh salt")
	}
	if device.KeyWrapped == wrapped1 {
		t.Fatal("--rewrap must produce different ciphertext")
	}
	if _, err := unwrapHostKey("first", device.KeyWrapped, device.KeySalt); err == nil {
		t.Fatal("old passphrase must fail after --rewrap")
	}
	if got, err := unwrapHostKey("second", device.KeyWrapped, device.KeySalt); err != nil || got != device.EncryptionKey {
		t.Fatalf("new passphrase must recover host key, got %q err %v", got, err)
	}
}

func TestEnsureHostKeyWithoutPassphraseDoesNotGenerate(t *testing.T) {
	device := &DeviceConfig{}
	changed, err := ensureHostKey(device, "", false)
	if !errors.Is(err, errNoPassphrase) {
		t.Fatalf("err = %v, want errNoPassphrase", err)
	}
	if changed || device.EncryptionKey != "" {
		t.Fatal("without a passphrase the client must stay in no-key mode")
	}
}

func TestResolvePassphrasePriority(t *testing.T) {
	t.Setenv("OPENCODE_TUNNEL_PASSPHRASE", "from-env")

	got, err := resolvePassphrase("from-flag")
	if err != nil || got != "from-flag" {
		t.Fatalf("flag passphrase = %q, %v", got, err)
	}

	got, err = resolvePassphrase("")
	if err != nil || got != "from-env" {
		t.Fatalf("env passphrase = %q, %v", got, err)
	}
}

func TestResolvePassphraseNonInteractive(t *testing.T) {
	t.Setenv("OPENCODE_TUNNEL_PASSPHRASE", "")
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = old })

	if _, err := resolvePassphrase(""); !errors.Is(err, errNoPassphrase) {
		t.Fatalf("err = %v, want errNoPassphrase", err)
	}
}

func TestReportHostKeySendsWrappedMaterial(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	device := &DeviceConfig{HostID: "s1", KeyWrapped: "wrapped", KeySalt: "salt", KeyKdf: keyKdfScrypt}
	if err := reportHostKey(srv.URL, "tok", device.HostID, device); err != nil {
		t.Fatalf("reportHostKey: %v", err)
	}
	if gotPath != "/api/hosts/s1/key" {
		t.Fatalf("path = %q, want /api/hosts/s1/key", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization = %q, want Bearer tok", gotAuth)
	}
	if gotBody["key_wrapped"] != "wrapped" || gotBody["key_salt"] != "salt" || gotBody["key_kdf"] != keyKdfScrypt {
		t.Fatalf("unexpected report body: %v", gotBody)
	}
}

func TestReportHostKeyStopsOnCredentialError(t *testing.T) {
	withFastKeyReportRetry(t)

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	device := &DeviceConfig{HostID: "s1", KeyWrapped: "w", KeySalt: "s", KeyKdf: keyKdfScrypt}
	if err := reportHostKey(srv.URL, "tok", "s1", device); err == nil {
		t.Fatal("403 must surface an error")
	}
	if calls != 1 {
		t.Fatalf("403 must not be retried, calls = %d", calls)
	}
}

func TestReportHostKeyRetriesServerErrorsThreeTimes(t *testing.T) {
	withFastKeyReportRetry(t)

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	device := &DeviceConfig{HostID: "s1", KeyWrapped: "w", KeySalt: "s", KeyKdf: keyKdfScrypt}
	if err := reportHostKey(srv.URL, "tok", "s1", device); err == nil {
		t.Fatal("5xx must surface an error after retries")
	}
	if calls != keyReportAttempts {
		t.Fatalf("calls = %d, want %d", calls, keyReportAttempts)
	}
}

func withFastKeyReportRetry(t *testing.T) {
	t.Helper()
	old := keyReportRetryDelay
	keyReportRetryDelay = 0
	t.Cleanup(func() { keyReportRetryDelay = old })
}

func TestPrepareHostKeyPersistsAndContinuesOnReportFailure(t *testing.T) {
	dir := withTempConfigDir(t)
	withFastKeyReportRetry(t)

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	device := &DeviceConfig{
		DeviceName: "box", RelayURL: srv.URL, Subdomain: "s1", HostID: "s1",
		AuthUser: "u", AuthPassword: "p",
	}
	prepareHostKey(srv.URL, "tok", device, "topsecret", false)

	if calls != keyReportAttempts {
		t.Fatalf("report calls = %d, want %d", calls, keyReportAttempts)
	}
	saved, err := loadDeviceConfig()
	if err != nil {
		t.Fatalf("device.json must survive report failure: %v", err)
	}
	if saved.KeyWrapped == "" || saved.KeySalt == "" || saved.KeyKdf != keyKdfScrypt {
		t.Fatalf("wrapped material not persisted: %+v", saved)
	}
	if saved.EncryptionKey == "" {
		t.Fatal("host key must be generated on first wrap")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, deviceFileName))
	if strings.Contains(string(raw), "topsecret") {
		t.Fatal("passphrase must never be written to disk")
	}
}

func TestPrepareHostKeySkipsWithoutPassphrase(t *testing.T) {
	dir := withTempConfigDir(t)
	t.Setenv("OPENCODE_TUNNEL_PASSPHRASE", "")
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = old })

	device := &DeviceConfig{
		DeviceName: "box", RelayURL: "http://relay", Subdomain: "s1", HostID: "s1",
		AuthUser: "u", AuthPassword: "p",
	}
	err := prepareHostKey("http://relay", "tok", device, "", false)
	if !errors.Is(err, errNoPassphrase) {
		t.Fatalf("err = %v, want errNoPassphrase", err)
	}
	if device.KeyWrapped != "" || device.EncryptionKey != "" {
		t.Fatal("no-key mode must not generate or wrap material")
	}
	if _, statErr := os.Stat(filepath.Join(dir, deviceFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("device.json should not be created, stat err = %v", statErr)
	}
}

// --- 第 5 组：--attach / --install / status ---

func TestParseStartArgsInstall(t *testing.T) {
	opts := parseStartArgs([]string{"start", "--install"})
	if !opts.Install {
		t.Fatalf("--install not parsed: %+v", opts)
	}
	if parseStartArgs([]string{"start"}).Install {
		t.Fatal("--install must default to false")
	}
}

func stubLocalServerSeams(t *testing.T) (probed *[]string, spawned *int) {
	t.Helper()
	probeCalls := []string{}
	spawnCalls := 0
	oldProbe, oldEnsure := probeLocalServer, ensureOpenCodeRunning
	probeLocalServer = func(port string) error {
		probeCalls = append(probeCalls, port)
		return nil
	}
	ensureOpenCodeRunning = func(port string) { spawnCalls++ }
	t.Cleanup(func() {
		probeLocalServer = oldProbe
		ensureOpenCodeRunning = oldEnsure
	})
	return &probeCalls, &spawnCalls
}

func TestAttachProbesInsteadOfSpawning(t *testing.T) {
	probed, spawned := stubLocalServerSeams(t)

	if err := prepareLocalServer(startOptions{Attach: true}, "5000"); err != nil {
		t.Fatalf("prepareLocalServer --attach: %v", err)
	}
	if len(*probed) != 1 || (*probed)[0] != "5000" {
		t.Fatalf("probe calls = %v, want [5000]", *probed)
	}
	if *spawned != 0 {
		t.Fatalf("--attach must not spawn or reuse-detect opencode, spawn calls = %d", *spawned)
	}
}

func TestAttachFailsWhenProbeFails(t *testing.T) {
	probed, spawned := stubLocalServerSeams(t)
	probeLocalServer = func(port string) error {
		*probed = append(*probed, port)
		return errors.New("connection refused")
	}

	err := prepareLocalServer(startOptions{Attach: true}, "5000")
	if err == nil {
		t.Fatal("--attach with a dead port must fail")
	}
	if !strings.Contains(err.Error(), "--attach 指定端口 5000 无响应") {
		t.Fatalf("error = %q, want attach hint", err.Error())
	}
	if *spawned != 0 {
		t.Fatalf("--attach must never spawn opencode, spawn calls = %d", *spawned)
	}
}

func TestWithoutAttachKeepsSpawnBehaviour(t *testing.T) {
	probed, spawned := stubLocalServerSeams(t)

	if err := prepareLocalServer(startOptions{}, "5000"); err != nil {
		t.Fatalf("prepareLocalServer: %v", err)
	}
	if len(*probed) != 0 {
		t.Fatalf("default path must not probe, probes = %v", *probed)
	}
	if *spawned != 1 {
		t.Fatalf("spawn calls = %d, want 1", *spawned)
	}
}

// installTestServer records the registration and key-report calls that
// installAutoStart makes.
type installTestServer struct {
	registered int32
	reported   int32
	keyBody    map[string]string
}

func newInstallTestServer(t *testing.T) (*httptest.Server, *installTestServer) {
	t.Helper()
	rec := &installTestServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/devices":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
		case r.Method == http.MethodPost && r.URL.Path == "/api/devices":
			atomic.AddInt32(&rec.registered, 1)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":1,"name":"box","subdomain":"s1","auth_user":"u","auth_password":"p"}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/hosts/s1/key":
			atomic.AddInt32(&rec.reported, 1)
			rec.keyBody = map[string]string{}
			json.NewDecoder(r.Body).Decode(&rec.keyBody)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func stubAutoStart(t *testing.T) *[]string {
	t.Helper()
	calls := []string{}
	old := setupAutoStart
	setupAutoStart = func(localPort, relay string, attach bool) error {
		calls = append(calls, localPort)
		return nil
	}
	t.Cleanup(func() { setupAutoStart = old })
	return &calls
}

func TestInstallRegistersHostAndWrapsKey(t *testing.T) {
	withTempConfigDir(t)
	withFastKeyReportRetry(t)
	t.Setenv("OPENCODE_TUNNEL_PASSPHRASE", "")

	srv, rec := newInstallTestServer(t)
	autoStart := stubAutoStart(t)

	opts := startOptions{Relay: srv.URL, Token: "tok", Name: "box", Passphrase: "topsecret"}
	if err := installAutoStart(opts, srv.URL, "box", "5000"); err != nil {
		t.Fatalf("installAutoStart: %v", err)
	}

	if rec.registered != 1 {
		t.Fatalf("registrations = %d, want 1", rec.registered)
	}
	if rec.reported != 1 {
		t.Fatalf("key reports = %d, want 1", rec.reported)
	}
	if rec.keyBody["key_wrapped"] == "" || rec.keyBody["key_kdf"] != keyKdfScrypt {
		t.Fatalf("unexpected reported key material: %v", rec.keyBody)
	}

	device, err := loadDeviceConfig()
	if err != nil {
		t.Fatalf("device.json must be written: %v", err)
	}
	if device.HostID == "" || device.KeyWrapped == "" {
		t.Fatalf("incomplete device.json: %+v", device)
	}

	if len(*autoStart) != 1 || (*autoStart)[0] != "5000" {
		t.Fatalf("setupAutoStart calls = %v, want [5000]", *autoStart)
	}
}

func TestInstallWithoutPassphraseFailsNonInteractive(t *testing.T) {
	withTempConfigDir(t)
	t.Setenv("OPENCODE_TUNNEL_PASSPHRASE", "")
	oldTTY := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = oldTTY })

	srv, rec := newInstallTestServer(t)
	autoStart := stubAutoStart(t)

	opts := startOptions{Relay: srv.URL, Token: "tok", Name: "box"}
	err := installAutoStart(opts, srv.URL, "box", "5000")
	if !errors.Is(err, errNoPassphrase) {
		t.Fatalf("err = %v, want errNoPassphrase", err)
	}
	if rec.reported != 0 {
		t.Fatalf("key must not be reported without a passphrase, reports = %d", rec.reported)
	}
	if len(*autoStart) != 0 {
		t.Fatalf("--install must not configure autostart after failure, calls = %v", *autoStart)
	}
}

func TestInstallReusesCompleteIdentity(t *testing.T) {
	withTempConfigDir(t)
	autoStart := stubAutoStart(t)

	if err := saveDeviceConfig(&DeviceConfig{
		DeviceName: "box", RelayURL: "http://relay", Subdomain: "s1", HostID: "s1",
		AuthUser: "u", AuthPassword: "p",
		KeyWrapped: "wrapped", KeySalt: "salt", KeyKdf: keyKdfScrypt,
	}); err != nil {
		t.Fatalf("saveDeviceConfig: %v", err)
	}

	if err := installAutoStart(startOptions{}, "http://relay", "box", "5000"); err != nil {
		t.Fatalf("installAutoStart: %v", err)
	}
	if len(*autoStart) != 1 {
		t.Fatalf("setupAutoStart calls = %d, want 1", len(*autoStart))
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestCmdStatusShowsHostIDAndE2EKey(t *testing.T) {
	withTempConfigDir(t)
	if err := saveAuthConfig(&AuthConfig{RelayURL: "http://relay", Token: "tok", Email: "e@x"}); err != nil {
		t.Fatalf("saveAuthConfig: %v", err)
	}
	if err := saveDeviceConfig(&DeviceConfig{
		DeviceName: "box", RelayURL: "http://relay", Subdomain: "s1", HostID: "s1",
		AuthUser: "u", AuthPassword: "p",
		KeyWrapped: "wrapped", KeySalt: "salt", KeyKdf: keyKdfScrypt,
	}); err != nil {
		t.Fatalf("saveDeviceConfig: %v", err)
	}

	out := captureStdout(t, cmdStatus)
	if !strings.Contains(out, "Host ID") || !strings.Contains(out, "s1") {
		t.Fatalf("status must show Host ID, got:\n%s", out)
	}
	if !strings.Contains(out, "E2E key: present") {
		t.Fatalf("status must show E2E key presence, got:\n%s", out)
	}
}

func TestCmdStatusShowsMissingE2EKey(t *testing.T) {
	withTempConfigDir(t)
	saveTestDevice(t, "http://relay")

	out := captureStdout(t, cmdStatus)
	if !strings.Contains(out, "E2E key: missing") {
		t.Fatalf("status must report a missing E2E key, got:\n%s", out)
	}
}

// --- 第 5 组：事件流订阅/重连 ---

// withFastSSEReconnect shrinks the production reconnect pause so the tests do
// not have to wait the real 3s between subscription attempts.
func withFastSSEReconnect(t *testing.T) {
	t.Helper()
	old := sseReconnectDelay
	sseReconnectDelay = 10 * time.Millisecond
	t.Cleanup(func() { sseReconnectDelay = old })
}

// sseTestSink is a fake relay tunnel: it accepts the client's WebSocket and
// exposes every forwarded SSE event, so tests can assert on what the client
// would have delivered upstream.
func sseTestSink(t *testing.T) (*websocket.Conn, <-chan SSEEvent) {
	t.Helper()

	events := make(chan SSEEvent, 32)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var tunnelEvent TunnelEvent
			if err := json.Unmarshal(msg, &tunnelEvent); err != nil {
				continue
			}
			var event SSEEvent
			if err := json.Unmarshal([]byte(tunnelEvent.Data), &event); err != nil {
				continue
			}
			select {
			case events <- event:
			default:
			}
		}
	}))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial test tunnel: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return conn, events
}

// newSSETestClient builds a TunnelClient whose local OpenCode endpoint is the
// given httptest server and whose upstream tunnel is the fake relay sink.
func newSSETestClient(t *testing.T, localServerURL, subdomain string) (*TunnelClient, <-chan SSEEvent) {
	t.Helper()

	u, err := url.Parse(localServerURL)
	if err != nil {
		t.Fatalf("parse local server URL: %v", err)
	}
	conn, events := sseTestSink(t)

	client := &TunnelClient{
		config:     &DeviceConfig{RelayURL: "http://relay.invalid", Subdomain: subdomain},
		localPort:  u.Port(),
		conn:       conn,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
	return client, events
}

func waitForSSEEvent(t *testing.T, events <-chan SSEEvent, wantType string, timeout time.Duration) {
	t.Helper()

	deadline := time.After(timeout)
	for {
		select {
		case event := <-events:
			if event.Type == wantType {
				return
			}
		case <-deadline:
			t.Fatalf("timed out after %v waiting for SSE event %q", timeout, wantType)
		}
	}
}

// writeSSEEvent emits one SSE frame of the shape OpenCode produces.
func writeSSEEvent(t *testing.T, w http.ResponseWriter, eventType string) {
	t.Helper()

	fmt.Fprintf(w, "data: {\"payload\":{\"type\":%q}}\n\n", eventType)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// A dropped local event stream must be resubscribed automatically and keep
// forwarding events, otherwise a restarted OpenCode silently loses the UI feed.
func TestSubscribeSSEReconnectsAfterStreamClose(t *testing.T) {
	withFastSSEReconnect(t)

	var requests int32
	reconnected := make(chan struct{})
	var reconnectedOnce sync.Once

	eventSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/event" {
			http.NotFound(w, r)
			return
		}
		n := atomic.AddInt32(&requests, 1)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		if n == 1 {
			// First subscription: deliver one event, then drop the stream.
			writeSSEEvent(t, w, "first")
			return
		}

		reconnectedOnce.Do(func() { close(reconnected) })
		writeSSEEvent(t, w, "second")
	}))
	t.Cleanup(eventSrv.Close)

	client, events := newSSETestClient(t, eventSrv.URL, "s1")
	client.startSSE()
	t.Cleanup(client.Stop)

	waitForSSEEvent(t, events, "first", 10*time.Second)

	select {
	case <-reconnected:
	case <-time.After(3 * time.Second):
		t.Fatalf("subscribeSSE did not resubscribe after the stream closed (requests=%d)", atomic.LoadInt32(&requests))
	}

	// The second subscription must keep feeding the tunnel.
	waitForSSEEvent(t, events, "second", 10*time.Second)
}

// Identity changes (re-register / --reset) build a new TunnelClient. Stopping
// the old instance must end its subscription and leave the new one working.
func TestStopEndsSubscriptionBeforeIdentityChange(t *testing.T) {
	withFastSSEReconnect(t)

	releaseOld := make(chan struct{})
	oldSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSEEvent(t, w, "old")
		// Keep the stream open and idle: only the stop signal may end it.
		select {
		case <-releaseOld:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(releaseOld)
		oldSrv.Close()
	})

	sendMore := make(chan struct{})
	releaseNew := make(chan struct{})
	newSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSEEvent(t, w, "new-1")
		select {
		case <-sendMore:
			writeSSEEvent(t, w, "new-2")
			<-releaseNew
		case <-releaseNew:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(releaseNew)
		newSrv.Close()
	})

	oldClient, oldEvents := newSSETestClient(t, oldSrv.URL, "old-subdomain")
	oldClient.startSSE()
	waitForSSEEvent(t, oldEvents, "old", 10*time.Second)

	newClient, newEvents := newSSETestClient(t, newSrv.URL, "new-subdomain")
	newClient.startSSE()
	t.Cleanup(newClient.Stop)
	waitForSSEEvent(t, newEvents, "new-1", 10*time.Second)

	// Stopping the old identity must not hang on an idle stream...
	stopped := make(chan struct{})
	go func() {
		oldClient.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() did not return: the old subscription outlived its stop signal")
	}

	select {
	case <-oldClient.sseStopChan:
	default:
		t.Fatal("Stop() must close the subscription stop channel")
	}

	// ...and must not disturb the subscription of the new identity.
	close(sendMore)
	waitForSSEEvent(t, newEvents, "new-2", 10*time.Second)
}

// Stop() must not return before the SSE goroutine has exited, otherwise a
// re-registration leaves the previous subscription forwarding stale events.
func TestStopWaitsForSubscriptionExit(t *testing.T) {
	release := make(chan struct{})
	eventSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSEEvent(t, w, "tick")
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		eventSrv.Close()
	})

	client, _ := newSSETestClient(t, eventSrv.URL, "s1")
	client.sseStopChan = make(chan struct{})
	client.sseStopped = false

	exited := make(chan struct{})
	client.sseWaitGroup.Add(1)
	go func() {
		// Registered before close(exited) so close runs first (defers are LIFO):
		// Wait() returning therefore guarantees the goroutine has finished.
		defer client.sseWaitGroup.Done()
		defer close(exited)
		client.subscribeSSE(client.sseStopChan)
	}()

	stopped := make(chan struct{})
	go func() {
		client.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() did not return while the stream was idle")
	}

	select {
	case <-exited:
	default:
		t.Fatal("Stop() returned before the SSE goroutine exited")
	}
}

// --- 独立审查修复回归测试 ---

// trackingReadCloser records how often a failed-handshake response body is
// closed, so the connect() cleanup path can be asserted without measuring FDs.
type trackingReadCloser struct {
	reads  int32
	closes int32
}

func (b *trackingReadCloser) Read(p []byte) (int, error) {
	atomic.AddInt32(&b.reads, 1)
	return 0, io.EOF
}

func (b *trackingReadCloser) Close() error {
	atomic.AddInt32(&b.closes, 1)
	return nil
}

func TestConnectClosesHandshakeFailureBody(t *testing.T) {
	withTempConfigDir(t)

	body := &trackingReadCloser{}
	old := wsDial
	wsDial = func(*websocket.Dialer, string, http.Header) (*websocket.Conn, *http.Response, error) {
		return nil, &http.Response{StatusCode: http.StatusInternalServerError, Body: body}, websocket.ErrBadHandshake
	}
	t.Cleanup(func() { wsDial = old })

	client := &TunnelClient{
		config: &DeviceConfig{
			RelayURL: "http://relay.example", Subdomain: "s1",
			AuthUser: "u", AuthPassword: "p",
		},
		localPort: "4096",
	}

	// Retries must not accumulate leaked response bodies.
	for i := 0; i < 3; i++ {
		err := client.connect()
		var statusErr *httpStatusError
		if !errors.As(err, &statusErr) || statusErr.Status != http.StatusInternalServerError {
			t.Fatalf("attempt %d: err = %v, want *httpStatusError with status 500", i, err)
		}
	}
	if got := atomic.LoadInt32(&body.closes); got != 3 {
		t.Fatalf("response body closed %d times, want 3", got)
	}
}

func TestInstallRewrapRefreshesWrappedKey(t *testing.T) {
	withTempConfigDir(t)
	withFastKeyReportRetry(t)

	hostKey, err := generateEncryptionKey()
	if err != nil {
		t.Fatalf("generateEncryptionKey: %v", err)
	}
	const oldSalt = "b2xkLXNhbHQ="
	if err := saveDeviceConfig(&DeviceConfig{
		DeviceName: "box", HostName: "box", RelayURL: "http://relay",
		Subdomain: "s1", HostID: "s1", AuthUser: "u", AuthPassword: "p",
		EncryptionKey: hostKey,
		KeyWrapped:    "old-wrapped", KeySalt: oldSalt, KeyKdf: keyKdfScrypt,
	}); err != nil {
		t.Fatalf("saveDeviceConfig: %v", err)
	}

	srv, rec := newInstallTestServer(t)
	autoStart := stubAutoStart(t)

	opts := startOptions{Relay: srv.URL, Token: "tok", Name: "box", Passphrase: "new-pass", Rewrap: true}
	if err := installAutoStart(opts, srv.URL, "box", "5000"); err != nil {
		t.Fatalf("installAutoStart --rewrap: %v", err)
	}

	if rec.reported != 1 {
		t.Fatalf("key reports = %d, want 1 for --rewrap", rec.reported)
	}
	if rec.keyBody["key_salt"] == oldSalt {
		t.Fatal("--rewrap must report a fresh salt")
	}

	saved, err := loadDeviceConfig()
	if err != nil {
		t.Fatalf("loadDeviceConfig: %v", err)
	}
	if saved.KeySalt == oldSalt {
		t.Fatal("--rewrap must persist a fresh salt")
	}
	if got, err := unwrapHostKey("new-pass", saved.KeyWrapped, saved.KeySalt); err != nil || got != hostKey {
		t.Fatalf("new passphrase must recover host key, got %q err %v", got, err)
	}
	if len(*autoStart) != 1 {
		t.Fatalf("setupAutoStart calls = %d, want 1", len(*autoStart))
	}
}

func TestReregisterPreservesWrappedKeyMaterial(t *testing.T) {
	withTempConfigDir(t)
	withFastKeyReportRetry(t)
	t.Setenv("OPENCODE_RELAY_TOKEN", "tok")

	hostKey, err := generateEncryptionKey()
	if err != nil {
		t.Fatalf("generateEncryptionKey: %v", err)
	}
	const oldWrapped, oldSalt = "wrapped-old", "c2FsdC1vbGQ="

	var gets, posts, puts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/devices":
			atomic.AddInt32(&gets, 1)
			w.Write([]byte(`[{"id":9,"name":"box","subdomain":"s1","auth_user":"u","auth_password":"p"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/devices":
			atomic.AddInt32(&posts, 1)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":10,"name":"box","subdomain":"s2","auth_user":"u","auth_password":"p"}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/hosts/s1/key":
			atomic.AddInt32(&puts, 1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if err := saveDeviceConfig(&DeviceConfig{
		DeviceName: "box", HostName: "box", RelayURL: srv.URL,
		Subdomain: "s1", HostID: "s1", AuthUser: "u", AuthPassword: "p",
		EncryptionKey: hostKey,
		KeyWrapped:    oldWrapped, KeySalt: oldSalt, KeyKdf: keyKdfScrypt,
	}); err != nil {
		t.Fatalf("saveDeviceConfig: %v", err)
	}

	oldRunner := tunnelRunner
	calls := 0
	tunnelRunner = func(*DeviceConfig, string) error {
		calls++
		if calls == 1 {
			return &httpStatusError{Op: "verify", Status: http.StatusNotFound}
		}
		return nil
	}
	t.Cleanup(func() { tunnelRunner = oldRunner })

	oldEnsure := ensureOpenCodeRunning
	ensureOpenCodeRunning = func(string) {}
	t.Cleanup(func() { ensureOpenCodeRunning = oldEnsure })

	oldArgs := os.Args
	os.Args = []string{"tunnel-client", "start", "--relay", srv.URL}
	t.Cleanup(func() { os.Args = oldArgs })

	cmdStart()

	if calls != 2 {
		t.Fatalf("tunnel attempts = %d, want 2 (fail then re-register)", calls)
	}
	if posts != 0 {
		t.Fatalf("POST /api/devices calls = %d, want 0 for same-subdomain reuse", posts)
	}
	if puts == 0 {
		t.Fatal("re-registration must report the preserved wrapped key")
	}

	saved, err := loadDeviceConfig()
	if err != nil {
		t.Fatalf("loadDeviceConfig: %v", err)
	}
	if saved.KeyWrapped != oldWrapped || saved.KeySalt != oldSalt || saved.KeyKdf != keyKdfScrypt {
		t.Fatalf("wrapped material lost on re-registration: %+v", saved)
	}
	if saved.EncryptionKey != hostKey {
		t.Fatal("host key lost on re-registration")
	}
}

func TestCmdStartStopsOnPolicyClose(t *testing.T) {
	withTempConfigDir(t)
	t.Setenv("OPENCODE_RELAY_TOKEN", "")
	saveTestDevice(t, "http://relay")

	oldEnsure := ensureOpenCodeRunning
	ensureOpenCodeRunning = func(string) {}
	t.Cleanup(func() { ensureOpenCodeRunning = oldEnsure })

	oldRunner := tunnelRunner
	tunnelRunner = func(*DeviceConfig, string) error {
		return fmt.Errorf("read error: %w", &websocket.CloseError{
			Code: websocket.ClosePolicyViolation, Text: "denied",
		})
	}
	t.Cleanup(func() { tunnelRunner = oldRunner })

	sentinel := errors.New("fatal exit")
	var got string
	oldFatal := fatalExit
	fatalExit = func(format string, v ...interface{}) {
		got = fmt.Sprintf(format, v...)
		panic(sentinel)
	}
	t.Cleanup(func() { fatalExit = oldFatal })

	oldArgs := os.Args
	os.Args = []string{"tunnel-client", "start"}
	t.Cleanup(func() { os.Args = oldArgs })

	func() {
		defer func() {
			if r := recover(); r != sentinel {
				t.Fatalf("cmdStart did not terminate on a policy close, recover = %v", r)
			}
		}()
		cmdStart()
	}()

	if !strings.Contains(got, "服务端拒绝连接") {
		t.Fatalf("fatal message = %q, want policy refusal hint", got)
	}
}

func TestEnsureHostRejectsSameNameBoundToOtherDevice(t *testing.T) {
	withTempConfigDir(t)
	if err := saveDeviceConfig(&DeviceConfig{
		DeviceName: "box", HostName: "box", RelayURL: "http://relay",
		Subdomain: "local1", HostID: "local1", AuthUser: "u", AuthPassword: "p",
	}); err != nil {
		t.Fatalf("saveDeviceConfig: %v", err)
	}

	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/devices":
			w.Write([]byte(`[{"id":2,"name":"box","subdomain":"other1","auth_user":"ou","auth_password":"op"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/devices":
			atomic.AddInt32(&posts, 1)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	_, err := ensureHost(srv.URL, "tok", "box", false)
	if err == nil || !strings.Contains(err.Error(), "同名主机已绑定其他设备") {
		t.Fatalf("err = %v, want 同名主机已绑定其他设备", err)
	}
	if posts != 0 {
		t.Fatalf("POST /api/devices calls = %d, want 0 on cross-device conflict", posts)
	}
}

func TestEnsureHostReusesSameSubdomain(t *testing.T) {
	withTempConfigDir(t)
	if err := saveDeviceConfig(&DeviceConfig{
		DeviceName: "box", HostName: "box", RelayURL: "http://relay",
		Subdomain: "s1", HostID: "s1", AuthUser: "u", AuthPassword: "p",
	}); err != nil {
		t.Fatalf("saveDeviceConfig: %v", err)
	}

	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/devices":
			w.Write([]byte(`[{"id":9,"name":"box","subdomain":"s1","auth_user":"u1","auth_password":"p1"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/devices":
			atomic.AddInt32(&posts, 1)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	device, err := ensureHost(srv.URL, "tok", "box", false)
	if err != nil {
		t.Fatalf("ensureHost: %v", err)
	}
	if device.HostID != "s1" {
		t.Fatalf("HostID = %q, want s1", device.HostID)
	}
	if posts != 0 {
		t.Fatalf("POST /api/devices calls = %d, want 0 for same subdomain", posts)
	}
}

func TestProcessSSEEventJoinsMultiLineData(t *testing.T) {
	client, events := newSSETestClient(t, "http://127.0.0.1:1", "s1")

	// A single event whose JSON is split across two data: lines.
	client.processSSEEvent([]byte("data: {\"payload\":{\"type\":\ndata: \"multi\"}}"))

	waitForSSEEvent(t, events, "multi", 5*time.Second)
}

func TestBuildWebSocketURLLogOmitsCredentials(t *testing.T) {
	var buf bytes.Buffer
	old := debugLogger
	debugLogger = log.New(&buf, "", 0)
	t.Cleanup(func() { debugLogger = old })

	client := &TunnelClient{config: &DeviceConfig{
		RelayURL: "https://relay.example.com", Subdomain: "s1",
		AuthUser: "user", AuthPassword: "super-secret",
	}}

	got := client.buildWebSocketURL()
	if !strings.Contains(got, "auth_password=super-secret") {
		t.Fatalf("dial URL must still carry credentials, got %q", got)
	}

	logged := buf.String()
	if strings.Contains(logged, "super-secret") || strings.Contains(logged, "auth_password") {
		t.Fatalf("debug log leaked credentials: %s", logged)
	}
	if !strings.Contains(logged, "wss://relay.example.com/api/tunnel/s1") {
		t.Fatalf("debug log missing sanitized target: %s", logged)
	}
}

func TestGenerateEncryptionKeyPropagatesRandError(t *testing.T) {
	old := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("entropy unavailable") }
	t.Cleanup(func() { randRead = old })

	key, err := generateEncryptionKey()
	if err == nil {
		t.Fatal("rand.Read failure must surface as an error")
	}
	if key != "" {
		t.Fatalf("key = %q, want empty on failure", key)
	}
}

// ---------------------------------------------------------------------------
// Streaming responses and WebSocket forwarding (design D1/D2/D5).
// ---------------------------------------------------------------------------

// frameSink is a fake relay tunnel that decodes every frame the client writes,
// both typed v2 frames and legacy TunnelResponse frames.
func frameSink(t *testing.T) (*websocket.Conn, <-chan any) {
	t.Helper()

	frames := make(chan any, 256)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				close(frames)
				return
			}
			frame, err := tunnel.DecodeFrame(msg)
			if err != nil {
				continue
			}
			frames <- frame
		}
	}))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial test tunnel: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return conn, frames
}

func nextFrame(t *testing.T, frames <-chan any, timeout time.Duration) any {
	t.Helper()
	select {
	case frame, ok := <-frames:
		if !ok {
			t.Fatal("frame sink closed before a frame arrived")
		}
		return frame
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a tunnel frame")
		return nil
	}
}

// streamTestClient wires a client to the given local HTTP server and a fake
// relay sink.
func streamTestClient(t *testing.T, localURL string) (*TunnelClient, <-chan any) {
	t.Helper()

	u, err := url.Parse(localURL)
	if err != nil {
		t.Fatalf("parse local server URL: %v", err)
	}
	conn, frames := frameSink(t)
	client := &TunnelClient{
		config:     &DeviceConfig{},
		localPort:  u.Port(),
		conn:       conn,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		streamMode: true,
	}
	t.Cleanup(client.closeAllWs)
	return client, frames
}

// sseRequest builds a tunnel request that asks for an event stream, which is
// the only shape the relay forwards incrementally.
func sseRequest(id, path string) *TunnelRequest {
	return &TunnelRequest{
		ID:      id,
		Method:  "GET",
		Path:    path,
		Headers: map[string]string{"Accept": "text/event-stream"},
	}
}

// A streaming response must start with res-head and then deliver the body in
// ordered 32 KiB chunks, ending with res-end.
func TestHandleRequestStreamsLargeBodyInOrder(t *testing.T) {
	body := make([]byte, 100*1024+123)
	for i := range body {
		body[i] = byte(i % 251)
	}

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Local", "yes")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	t.Cleanup(local.Close)

	client, frames := streamTestClient(t, local.URL)
	done := make(chan struct{})
	go func() {
		client.handleRequest(sseRequest("r1", "/big"))
		close(done)
	}()

	head, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.ResHead)
	if !ok {
		t.Fatal("first frame must be res-head")
	}
	if head.ID != "r1" || head.StatusCode != http.StatusOK {
		t.Fatalf("res-head = %+v, want id r1 status 200", head)
	}
	if head.Headers["X-Local"] != "yes" {
		t.Fatalf("res-head lost local header: %+v", head.Headers)
	}

	var got []byte
	var wantSeq int64
	sawEnd := false
	for !sawEnd {
		switch frame := nextFrame(t, frames, 5*time.Second).(type) {
		case *tunnel.ResChunk:
			if frame.Seq != wantSeq {
				t.Fatalf("chunk seq = %d, want %d", frame.Seq, wantSeq)
			}
			if len(frame.Data) > 32*1024 {
				t.Fatalf("chunk size = %d, want <= 32 KiB", len(frame.Data))
			}
			wantSeq++
			got = append(got, frame.Data...)
		case *tunnel.ResEnd:
			sawEnd = true
		case *tunnel.ResError:
			t.Fatalf("unexpected res-error: %s", frame.Message)
		default:
			t.Fatalf("unexpected frame %T", frame)
		}
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("streamed body mismatch: got %d bytes, want %d", len(got), len(body))
	}
	<-done
}

// A local read failure must be reported as res-error, not silently truncated.
func TestHandleRequestStreamReadErrorSendsResError(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response writer is not a hijacker")
			return
		}
		conn, buf, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		// Promise 4096 bytes, deliver 7, then drop the connection.
		fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Length: 4096\r\nContent-Type: text/plain\r\n\r\npartial")
		buf.Flush()
	}))
	t.Cleanup(local.Close)

	client, frames := streamTestClient(t, local.URL)
	go client.handleRequest(sseRequest("r1", "/broken"))

	if _, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.ResHead); !ok {
		t.Fatal("first frame must be res-head")
	}
	for {
		switch frame := nextFrame(t, frames, 5*time.Second).(type) {
		case *tunnel.ResChunk:
			// Partial data may arrive before the failure is detected.
		case *tunnel.ResError:
			if frame.ID != "r1" || frame.Message == "" {
				t.Fatalf("res-error = %+v, want id r1 with a message", frame)
			}
			return
		case *tunnel.ResEnd:
			t.Fatal("res-end must not follow a read failure")
		}
	}
}

// A v2 tunnel must still answer ordinary requests with a single legacy
// TunnelResponse frame: the relay only streams SSE and drops typed frames for
// everything else. Streaming a plain request here used to time out at 120s.
func TestHandleRequestV2PlainRequestUsesLegacyPath(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello world")
	}))
	t.Cleanup(local.Close)

	client, frames := streamTestClient(t, local.URL)
	if !client.isStreamMode() {
		t.Fatal("test client must negotiate protocol 2")
	}

	done := make(chan struct{})
	go func() {
		client.handleRequest(&TunnelRequest{ID: "plain-1", Method: "GET", Path: "/hello"})
		close(done)
	}()

	select {
	case frame := <-frames:
		resp, ok := frame.(*tunnel.TunnelResponse)
		if !ok {
			t.Fatalf("v2 plain request wrote %T, want a legacy TunnelResponse", frame)
		}
		if resp.ID != "plain-1" || resp.StatusCode != http.StatusOK {
			t.Fatalf("legacy response = %+v", resp)
		}
		if string(resp.Body) != "hello world" {
			t.Fatalf("legacy body = %q, want hello world", resp.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("v2 plain request produced no frame within 2s")
	}
	<-done
}

// Without a v2 handshake the client keeps the single-frame legacy path.
func TestHandleRequestLegacySendsSingleFrame(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello world")
	}))
	t.Cleanup(local.Close)

	client, frames := streamTestClient(t, local.URL)
	client.streamMode = false

	client.handleRequest(&TunnelRequest{ID: "legacy-1", Method: "GET", Path: "/"})

	resp, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.TunnelResponse)
	if !ok {
		t.Fatal("legacy mode must write one TunnelResponse frame")
	}
	if resp.ID != "legacy-1" || resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy response = %+v", resp)
	}
	if string(resp.Body) != "hello world" {
		t.Fatalf("legacy body = %q, want hello world", resp.Body)
	}
}

// The handshake response decides the mode: >= 2 streams, missing/1 falls back.
func TestConnectNegotiatesStreamProtocol(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{name: "v2", header: "2", want: true},
		{name: "v3", header: "3", want: true},
		{name: "v1", header: "1", want: false},
		{name: "missing", header: "", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requestHeader := make(chan string, 1)
			upgrader := websocket.Upgrader{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestHeader <- r.Header.Get("X-Tunnel-Protocol")
				var responseHeader http.Header
				if tc.header != "" {
					responseHeader = http.Header{"X-Tunnel-Protocol": []string{tc.header}}
				}
				conn, err := upgrader.Upgrade(w, r, responseHeader)
				if err != nil {
					return
				}
				conn.Close()
			}))
			t.Cleanup(srv.Close)

			client := &TunnelClient{
				config:     &DeviceConfig{RelayURL: srv.URL, Subdomain: "s1"},
				localPort:  "1",
				httpClient: &http.Client{Timeout: time.Second},
			}
			client.connect()

			if got := <-requestHeader; got != "2" {
				t.Fatalf("client announced protocol %q, want 2", got)
			}
			if client.streamMode != tc.want {
				t.Fatalf("streamMode = %v, want %v for header %q", client.streamMode, tc.want, tc.header)
			}
		})
	}
}

type localWSInfo struct {
	header      http.Header
	subprotocol string
}

// wsTestServer is a local echo WebSocket endpoint that reports the handshake it
// saw and the close code it eventually received.
func wsTestServer(t *testing.T) (string, <-chan localWSInfo, <-chan int) {
	t.Helper()

	infoCh := make(chan localWSInfo, 1)
	closeCh := make(chan int, 1)
	upgrader := websocket.Upgrader{
		Subprotocols: []string{"proto.v1"},
		CheckOrigin:  func(*http.Request) bool { return true },
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		infoCh <- localWSInfo{header: r.Header.Clone(), subprotocol: conn.Subprotocol()}
		for {
			opcode, data, err := conn.ReadMessage()
			if err != nil {
				code := websocket.CloseAbnormalClosure
				var closeErr *websocket.CloseError
				if errors.As(err, &closeErr) {
					code = closeErr.Code
				}
				closeCh <- code
				return
			}
			if err := conn.WriteMessage(opcode, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), infoCh, closeCh
}

// ws-open dials the local server, filters relay credentials, negotiates the
// subprotocol and bridges both directions.
func TestHandleWsOpenForwardsFramesAndSubprotocol(t *testing.T) {
	wsURL, infoCh, closeCh := wsTestServer(t)

	client, frames := streamTestClient(t, "http://127.0.0.1:"+portOf(t, wsURL))
	client.handleWsOpen(&tunnel.WsOpen{
		ID:   "w1",
		Path: "/terminal",
		Headers: map[string]string{
			"X-Custom":      "yes",
			"Authorization": "Bearer relay-secret",
			"Cookie":        "session=relay-secret",
			"Connection":    "Upgrade",
		},
		Subprotocols: []string{"proto.v1"},
	})

	ack, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.WsAck)
	if !ok {
		t.Fatal("first frame after ws-open must be ws-ack")
	}
	if !ack.OK || ack.Status != http.StatusSwitchingProtocols || ack.Subprotocol != "proto.v1" {
		t.Fatalf("ws-ack = %+v", ack)
	}

	info := <-infoCh
	if info.subprotocol != "proto.v1" {
		t.Fatalf("local subprotocol = %q, want proto.v1", info.subprotocol)
	}
	if info.header.Get("X-Custom") != "yes" {
		t.Fatalf("custom header not forwarded: %v", info.header)
	}
	if info.header.Get("Authorization") != "" || info.header.Get("Cookie") != "" {
		t.Fatalf("relay credentials leaked to local WS: %v", info.header)
	}

	// relay -> local
	client.handleWsFrame(&tunnel.WsFrame{ID: "w1", Opcode: websocket.TextMessage, Data: []byte("ping")})

	// local echo -> relay
	echo, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.WsFrame)
	if !ok {
		t.Fatal("expected ws-frame from the local echo")
	}
	if echo.ID != "w1" || echo.Opcode != websocket.TextMessage || string(echo.Data) != "ping" {
		t.Fatalf("ws-frame = %+v", echo)
	}

	// relay -> local close propagates the code.
	client.handleWsClose(&tunnel.WsClose{ID: "w1", Code: websocket.CloseNormalClosure, Reason: "done"})
	select {
	case code := <-closeCh:
		if code != websocket.CloseNormalClosure {
			t.Fatalf("local close code = %d, want %d", code, websocket.CloseNormalClosure)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("local WS did not observe the relay close")
	}
}

func portOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return u.Port()
}

// A local dial failure is reported as ws-ack{ok:false,status:502}.
func TestHandleWsOpenDialFailureAcks502(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(local.Close)

	client, frames := streamTestClient(t, local.URL)
	client.handleWsOpen(&tunnel.WsOpen{ID: "w1", Path: "/missing"})

	ack, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.WsAck)
	if !ok {
		t.Fatal("expected ws-ack")
	}
	if ack.OK || ack.Status != http.StatusBadGateway || ack.Error == "" {
		t.Fatalf("ws-ack = %+v, want ok=false status=502 with an error", ack)
	}
}

// A duplicate ws-open for an id that is still open must be rejected.
func TestHandleWsOpenRejectsDuplicate(t *testing.T) {
	wsURL, _, _ := wsTestServer(t)
	client, frames := streamTestClient(t, "http://127.0.0.1:"+portOf(t, wsURL))

	client.handleWsOpen(&tunnel.WsOpen{ID: "w1", Path: "/terminal", Subprotocols: []string{"proto.v1"}})
	if ack, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.WsAck); !ok || !ack.OK {
		t.Fatalf("first ws-ack = %+v, ok=%v", ack, ok)
	}

	client.handleWsOpen(&tunnel.WsOpen{ID: "w1", Path: "/terminal"})
	ack, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.WsAck)
	if !ok {
		t.Fatal("expected a second ws-ack")
	}
	if ack.OK || ack.Status != http.StatusConflict {
		t.Fatalf("duplicate ws-ack = %+v, want ok=false status=409", ack)
	}
}

// A locally-initiated close is reported upstream with its close code.
func TestLocalWsCloseSendsWsClose(t *testing.T) {
	wsURL, infoCh, _ := wsTestServer(t)
	client, frames := streamTestClient(t, "http://127.0.0.1:"+portOf(t, wsURL))

	client.handleWsOpen(&tunnel.WsOpen{ID: "w1", Path: "/terminal", Subprotocols: []string{"proto.v1"}})
	if ack, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.WsAck); !ok || !ack.OK {
		t.Fatalf("ws-ack = %+v, ok=%v", ack, ok)
	}
	<-infoCh

	// Reach into the session to close it the way the local endpoint would.
	client.wsMu.Lock()
	sess := client.wsConns["w1"]
	client.wsMu.Unlock()
	if sess == nil || sess.conn == nil {
		t.Fatal("local ws session missing")
	}
	sess.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseGoingAway, "shutdown"), time.Now().Add(time.Second))

	closeFrame, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.WsClose)
	if !ok {
		t.Fatal("expected ws-close from the local close")
	}
	if closeFrame.ID != "w1" || closeFrame.Code != websocket.CloseGoingAway {
		t.Fatalf("ws-close = %+v, want id w1 code 1001", closeFrame)
	}
}

// req-cancel stops the local request and no further chunks are sent; cancelling
// twice is a no-op.
func TestHandleReqCancelStopsStream(t *testing.T) {
	started := make(chan struct{})
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "first")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(local.Close)

	client, frames := streamTestClient(t, local.URL)
	go client.handleRequest(sseRequest("s1", "/events"))

	if _, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.ResHead); !ok {
		t.Fatal("expected res-head")
	}
	chunk, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.ResChunk)
	if !ok || string(chunk.Data) != "first" {
		t.Fatalf("first chunk = %+v, ok=%v", chunk, ok)
	}
	<-started

	client.handleReqCancel(&tunnel.ReqCancel{ID: "s1"})
	client.handleReqCancel(&tunnel.ReqCancel{ID: "s1"}) // idempotent, must not panic

	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case frame := <-frames:
			if c, ok := frame.(*tunnel.ResChunk); ok {
				t.Fatalf("chunk after cancel: %q", c.Data)
			}
		case <-deadline:
			return
		}
	}
}

// Oversized response headers must be trimmed with a warning instead of being
// silently dropped or failing the whole stream.
func TestSendResHeadTruncatesOversizedHeaders(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Small", "kept")
		w.Header().Set("X-Huge", strings.Repeat("a", 3<<20))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(local.Close)

	client, frames := streamTestClient(t, local.URL)

	var logged bytes.Buffer
	oldOutput, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		log.SetFlags(oldFlags)
	})

	go client.handleRequest(sseRequest("r1", "/huge-headers"))

	head, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.ResHead)
	if !ok {
		t.Fatal("expected res-head")
	}
	if head.ID != "r1" || head.StatusCode != http.StatusOK {
		t.Fatalf("res-head = %+v, want id r1 status 200", head)
	}
	if _, present := head.Headers["X-Huge"]; present {
		t.Fatal("oversized header was not trimmed")
	}
	if head.Headers["X-Small"] != "kept" {
		t.Fatalf("small header lost during trimming: %+v", head.Headers)
	}
	if !strings.Contains(logged.String(), "dropped") {
		t.Fatalf("header trimming was not logged: %q", logged.String())
	}
}

// ---------------------------------------------------------------------------
// Streaming payload encryption (design D8).
// ---------------------------------------------------------------------------

// testStreamKey is a valid 32-byte AES-256 key in hex.
const testStreamKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func testBinaryPayload(n int) []byte {
	body := make([]byte, n)
	for i := range body {
		body[i] = byte(i % 256)
	}
	// Make sure the edge bytes the encryption layer must not special-case are
	// present.
	if n > 2 {
		body[0] = 0x00
		body[1] = 0xff
		body[n-1] = 0x00
	}
	return body
}

// A stream chunk is encrypted on its own and carries the raw
// nonce||ciphertext||tag layout; the head marks the stream as encrypted.
func TestHandleRequestStreamEncryptsChunks(t *testing.T) {
	body := testBinaryPayload(70*1024 + 7)

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	t.Cleanup(local.Close)

	client, frames := streamTestClient(t, local.URL)
	client.config.EncryptionKey = testStreamKey

	go client.handleRequest(sseRequest("e1", "/encrypted"))

	head, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.ResHead)
	if !ok {
		t.Fatal("first frame must be res-head")
	}
	if got := head.Headers["X-E2E-Stream"]; got != "1" {
		t.Fatalf("X-E2E-Stream = %q, want 1", got)
	}

	var got []byte
	sawEnd := false
	for !sawEnd {
		switch frame := nextFrame(t, frames, 5*time.Second).(type) {
		case *tunnel.ResChunk:
			plain, err := decryptSSEChunk(frame.Data, testStreamKey)
			if err != nil {
				t.Fatalf("chunk %d did not decrypt: %v", frame.Seq, err)
			}
			if want := len(plain) + 4 + 28; len(frame.Data) != want {
				t.Fatalf("chunk %d ciphertext = %d bytes, want %d (len+nonce+tag)", frame.Seq, len(frame.Data), want)
			}
			got = append(got, plain...)
		case *tunnel.ResEnd:
			sawEnd = true
		case *tunnel.ResError:
			t.Fatalf("unexpected res-error: %s", frame.Message)
		default:
			t.Fatalf("unexpected frame %T", frame)
		}
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("decrypted stream mismatch: got %d bytes, want %d", len(got), len(body))
	}
}

// Without a key the stream stays plaintext and says so explicitly.
func TestHandleRequestStreamWithoutKeyMarksPlaintext(t *testing.T) {
	body := []byte("data: plain\n\n")

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	t.Cleanup(local.Close)

	client, frames := streamTestClient(t, local.URL)
	go client.handleRequest(sseRequest("p1", "/events"))

	head, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.ResHead)
	if !ok {
		t.Fatal("first frame must be res-head")
	}
	if got := head.Headers["X-E2E-Stream"]; got != "0" {
		t.Fatalf("X-E2E-Stream = %q, want 0", got)
	}

	chunk, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.ResChunk)
	if !ok {
		t.Fatal("expected res-chunk")
	}
	if !bytes.Equal(chunk.Data, body) {
		t.Fatalf("chunk = %q, want plaintext %q", chunk.Data, body)
	}
	if _, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.ResEnd); !ok {
		t.Fatal("expected res-end")
	}
}

// Each encryption is independent: fresh nonce, raw layout, and only the right
// key decrypts.
func TestStreamPayloadRoundTrip(t *testing.T) {
	plain := testBinaryPayload(1024)

	first, err := encryptStreamPayload(plain, testStreamKey)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	second, err := encryptStreamPayload(plain, testStreamKey)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if len(first) != len(plain)+28 {
		t.Fatalf("ciphertext = %d bytes, want %d", len(first), len(plain)+28)
	}
	if bytes.Equal(first, second) {
		t.Fatal("two encryptions produced identical bytes; nonce is not random")
	}

	got, err := decryptStreamPayload(first, testStreamKey)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round trip mismatch: got %d bytes, want %d", len(got), len(plain))
	}

	wrongKey := strings.Repeat("ff", 32)
	if _, err := decryptStreamPayload(first, wrongKey); err == nil {
		t.Fatal("decrypt with the wrong key succeeded")
	}
}

// SSE chunks carry an explicit plaintext length so the app can decrypt them
// without relying on HTTP chunk boundaries.
func TestSSEChunkFraming(t *testing.T) {
	plain := testBinaryPayload(700)

	framed, err := encryptSSEChunk(plain, testStreamKey)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if len(framed) != 4+len(plain)+28 {
		t.Fatalf("framed = %d bytes, want %d", len(framed), 4+len(plain)+28)
	}
	if got := int(binary.BigEndian.Uint32(framed[:4])); got != len(plain) {
		t.Fatalf("length prefix = %d, want %d", got, len(plain))
	}

	back, err := decryptSSEChunk(framed, testStreamKey)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(back, plain) {
		t.Fatal("round trip mismatch")
	}

	// A truncated or mis-framed chunk must fail loudly rather than yield
	// partial plaintext.
	if _, err := decryptSSEChunk(framed[:3], testStreamKey); err == nil {
		t.Fatal("short chunk accepted")
	}
	bad := append([]byte{}, framed...)
	binary.BigEndian.PutUint32(bad[:4], uint32(len(plain)+1))
	if _, err := decryptSSEChunk(bad, testStreamKey); err == nil {
		t.Fatal("length mismatch accepted")
	}
}

// ws-open must report whether the session payloads are encrypted, and the
// bridge must encrypt both directions with the same key.
func TestHandleWsOpenEncryptsFrames(t *testing.T) {
	wsURL, infoCh, closeCh := wsTestServer(t)

	client, frames := streamTestClient(t, "http://127.0.0.1:"+portOf(t, wsURL))
	client.config.EncryptionKey = testStreamKey

	client.handleWsOpen(&tunnel.WsOpen{ID: "w1", Path: "/terminal", Subprotocols: []string{"proto.v1"}})

	ack, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.WsAck)
	if !ok {
		t.Fatal("first frame after ws-open must be ws-ack")
	}
	if !ack.OK || !ack.Encrypted {
		t.Fatalf("ws-ack = %+v, want ok=true encrypted=true", ack)
	}
	<-infoCh

	// relay -> local: the relay frame carries ciphertext whose plaintext starts
	// with the original opcode.
	sealed := append([]byte{byte(websocket.TextMessage)}, []byte("ping")...)
	encrypted, err := encryptStreamPayload(sealed, testStreamKey)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	client.handleWsFrame(&tunnel.WsFrame{ID: "w1", Opcode: websocket.BinaryMessage, Data: encrypted})

	// local echo -> relay: the client must encrypt what the local socket sent.
	echo, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.WsFrame)
	if !ok {
		t.Fatal("expected ws-frame from the local echo")
	}
	if bytes.Equal(echo.Data, []byte("ping")) {
		t.Fatal("ws-frame payload was forwarded in plaintext")
	}
	if echo.Opcode != websocket.BinaryMessage {
		t.Fatalf("encrypted frame opcode = %d, want binary", echo.Opcode)
	}
	plain, err := decryptStreamPayload(echo.Data, testStreamKey)
	if err != nil {
		t.Fatalf("forwarded frame did not decrypt: %v", err)
	}
	if len(plain) < 2 || plain[0] != byte(websocket.TextMessage) {
		t.Fatalf("sealed opcode = %v, want text", plain)
	}
	if string(plain[1:]) != "ping" {
		t.Fatalf("forwarded frame = %q, want ping", plain[1:])
	}

	client.handleWsClose(&tunnel.WsClose{ID: "w1", Code: websocket.CloseNormalClosure, Reason: "done"})
	select {
	case <-closeCh:
	case <-time.After(5 * time.Second):
		t.Fatal("local WS did not observe the relay close")
	}
}

// Without a key the ack says plaintext and frames pass through untouched.
func TestHandleWsOpenWithoutKeyMarksPlaintext(t *testing.T) {
	wsURL, infoCh, _ := wsTestServer(t)

	client, frames := streamTestClient(t, "http://127.0.0.1:"+portOf(t, wsURL))
	client.handleWsOpen(&tunnel.WsOpen{ID: "w1", Path: "/terminal", Subprotocols: []string{"proto.v1"}})

	ack, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.WsAck)
	if !ok || !ack.OK {
		t.Fatalf("ws-ack = %+v, ok=%v", ack, ok)
	}
	if ack.Encrypted {
		t.Fatal("ws-ack.Encrypted = true without a key")
	}
	<-infoCh

	client.handleWsFrame(&tunnel.WsFrame{ID: "w1", Opcode: websocket.TextMessage, Data: []byte("ping")})
	echo, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.WsFrame)
	if !ok || string(echo.Data) != "ping" {
		t.Fatalf("ws-frame = %+v, want plaintext ping", echo)
	}
}

// A payload that cannot be decrypted must close the session with 1011 instead
// of writing ciphertext to the local socket.
func TestWsFrameDecryptFailureCloses1011(t *testing.T) {
	wsURL, infoCh, closeCh := wsTestServer(t)

	client, frames := streamTestClient(t, "http://127.0.0.1:"+portOf(t, wsURL))
	client.config.EncryptionKey = "not-a-hex-key"

	client.handleWsOpen(&tunnel.WsOpen{ID: "w1", Path: "/terminal", Subprotocols: []string{"proto.v1"}})
	if ack, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.WsAck); !ok || !ack.OK {
		t.Fatalf("ws-ack = %+v, ok=%v", ack, ok)
	}
	<-infoCh

	client.handleWsFrame(&tunnel.WsFrame{ID: "w1", Opcode: websocket.TextMessage, Data: []byte("garbage")})

	closeFrame, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.WsClose)
	if !ok {
		t.Fatal("expected ws-close after a decrypt failure")
	}
	if closeFrame.ID != "w1" || closeFrame.Code != websocket.CloseInternalServerErr {
		t.Fatalf("ws-close = %+v, want id w1 code 1011", closeFrame)
	}
	select {
	case code := <-closeCh:
		if code != websocket.CloseInternalServerErr {
			t.Fatalf("local close code = %d, want 1011", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("local WS was not closed after the decrypt failure")
	}
}

// A frame the client cannot encrypt must not reach the relay in plaintext; the
// session closes with 1011.
func TestWsFrameEncryptFailureCloses1011(t *testing.T) {
	wsURL, infoCh, closeCh := wsTestServer(t)

	client, frames := streamTestClient(t, "http://127.0.0.1:"+portOf(t, wsURL))
	client.config.EncryptionKey = "not-a-hex-key"

	client.handleWsOpen(&tunnel.WsOpen{ID: "w1", Path: "/terminal", Subprotocols: []string{"proto.v1"}})
	if ack, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.WsAck); !ok || !ack.OK {
		t.Fatalf("ws-ack = %+v, ok=%v", ack, ok)
	}
	<-infoCh

	client.wsMu.Lock()
	sess := client.wsConns["w1"]
	client.wsMu.Unlock()
	if sess == nil || sess.conn == nil {
		t.Fatal("local ws session missing")
	}
	if err := sess.conn.WriteMessage(websocket.TextMessage, []byte("outbound")); err != nil {
		t.Fatalf("local write: %v", err)
	}

	closeFrame, ok := nextFrame(t, frames, 5*time.Second).(*tunnel.WsClose)
	if !ok {
		t.Fatal("expected ws-close after an encrypt failure")
	}
	if closeFrame.Code != websocket.CloseInternalServerErr {
		t.Fatalf("ws-close = %+v, want code 1011", closeFrame)
	}
	select {
	case code := <-closeCh:
		if code != websocket.CloseInternalServerErr {
			t.Fatalf("local close code = %d, want 1011", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("local WS was not closed after the encrypt failure")
	}
}

func TestApplyLocalAuth(t *testing.T) {
	client := &TunnelClient{localAuthUser: "opencode", localAuthPassword: "local-secret"}
	header := http.Header{}
	client.applyLocalAuth(header)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:local-secret"))
	if got := header.Get("Authorization"); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}

	empty := &TunnelClient{}
	other := http.Header{}
	empty.applyLocalAuth(other)
	if got := other.Get("Authorization"); got != "" {
		t.Fatalf("expected no Authorization header without local credentials, got %q", got)
	}
}

func TestNewLocalRequestAppliesLocalAuth(t *testing.T) {
	client := &TunnelClient{config: &DeviceConfig{}, localPort: "4096", localAuthUser: "opencode", localAuthPassword: "local-secret"}
	req, status, message := client.newLocalRequest(&TunnelRequest{ID: "test", Method: http.MethodGet, Path: "/api/info"})
	if req == nil {
		t.Fatalf("newLocalRequest failed: %d %s", status, message)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:local-secret"))
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

// TestDispatchSafeBranches covers the dispatch branches that return without
// starting a tunnel. The implicit-start (-relay ...) and known subcommand
// branches intentionally call cmdStart/cmdStatus/... which would start a real
// tunnel or hit the network, so they are not exercised here; their parsing is
// covered by TestParseStartArgs and the command-level tests.
func TestDispatchSafeBranches(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantOutput []string
	}{
		{name: "help", args: []string{"help"}, wantCode: 0, wantOutput: []string{"Usage:", "version"}},
		{name: "short help", args: []string{"-h"}, wantCode: 0, wantOutput: []string{"Usage:"}},
		{name: "long help", args: []string{"--help"}, wantCode: 0, wantOutput: []string{"Usage:"}},
		{name: "version", args: []string{"version"}, wantCode: 0, wantOutput: []string{"tunnel-client"}},
		{name: "short version", args: []string{"-v"}, wantCode: 0, wantOutput: []string{"tunnel-client"}},
		{name: "long version", args: []string{"--version"}, wantCode: 0, wantOutput: []string{"tunnel-client"}},
		{name: "unknown command", args: []string{"bogus"}, wantCode: 1, wantOutput: []string{"Unknown command: bogus", "Usage:"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			out := captureStdout(t, func() { code = dispatch(tc.args) })
			if code != tc.wantCode {
				t.Fatalf("dispatch(%v) = %d, want %d; output:\n%s", tc.args, code, tc.wantCode, out)
			}
			for _, want := range tc.wantOutput {
				if !strings.Contains(out, want) {
					t.Fatalf("dispatch(%v) output missing %q:\n%s", tc.args, want, out)
				}
			}
		})
	}
}

func TestPrintVersionUsesInjectedBuildInfo(t *testing.T) {
	oldVersion, oldTime, oldCommit := buildVersion, buildTime, buildCommit
	buildVersion, buildTime, buildCommit = "1.2.3", "2026-10-23T00:00:00Z", "abc1234"
	t.Cleanup(func() {
		buildVersion, buildTime, buildCommit = oldVersion, oldTime, oldCommit
	})

	out := captureStdout(t, printVersion)
	want := "tunnel-client 1.2.3 (built: 2026-10-23T00:00:00Z, commit: abc1234)"
	if !strings.Contains(out, want) {
		t.Fatalf("printVersion output %q does not contain %q", out, want)
	}
}

// TestDispatchRoutesToStart covers the ISSUE-01 regression point: a leading
// flag (no "start" token) and an explicit "start" must both reach the start
// path exactly once, while help/version must not start anything.
func TestDispatchRoutesToStart(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantCall int
	}{
		{name: "implicit start with relay flag", args: []string{"-relay", "https://example.test"}, wantCode: 0, wantCall: 1},
		{name: "implicit start with port flag", args: []string{"-port", "4096"}, wantCode: 0, wantCall: 1},
		{name: "explicit start", args: []string{"start", "-port", "4096"}, wantCode: 0, wantCall: 1},
		{name: "help does not start", args: []string{"--help"}, wantCode: 0, wantCall: 0},
		{name: "version does not start", args: []string{"--version"}, wantCode: 0, wantCall: 0},
		{name: "single dash version does not start", args: []string{"-version"}, wantCode: 0, wantCall: 0},
		{name: "single dash help does not start", args: []string{"-help"}, wantCode: 0, wantCall: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			old := startCommand
			calls := 0
			startCommand = func() { calls++ }
			t.Cleanup(func() { startCommand = old })

			code := captureStdout(t, func() {
				got := dispatch(tc.args)
				if got != tc.wantCode {
					t.Fatalf("dispatch(%v) = %d, want %d", tc.args, got, tc.wantCode)
				}
			})
			_ = code
			if calls != tc.wantCall {
				t.Fatalf("dispatch(%v) called startCommand %d time(s), want %d", tc.args, calls, tc.wantCall)
			}
		})
	}
}

// --- systemd 自启 unit 生成（design D4） ---

// systemdExecStart 取出生成 unit 中的 ExecStart= 行。
func systemdExecStart(t *testing.T, unit string) string {
	t.Helper()
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			return strings.TrimPrefix(line, "ExecStart=")
		}
	}
	t.Fatalf("unit has no ExecStart line:\n%s", unit)
	return ""
}

func TestBuildSystemdUnit(t *testing.T) {
	tests := []struct {
		name      string
		localPort string
		relay     string
		attach    bool
		want      []string
		notWant   []string
	}{
		{
			name:      "default relay and no attach",
			localPort: "4096",
			relay:     "https://example.test",
			want:      []string{"start -relay https://example.test -port 4096"},
			notWant:   []string{"--attach"},
		},
		{
			name:      "attach adds flag",
			localPort: "4096",
			relay:     "https://example.test",
			attach:    true,
			want:      []string{"start -relay https://example.test --attach -port 4096"},
		},
		{
			name:      "custom port",
			localPort: "5123",
			relay:     "http://relay.local:14096",
			want:      []string{"-relay http://relay.local:14096", "-port 5123"},
			notWant:   []string{"-port 4096"},
		},
		{
			// 空 relay 必须回退到 defaultRelay 并显式写入，否则开机自启会
			// 依赖客户端自己的默认值，和安装时解析结果脱节。
			name:      "empty relay falls back to default",
			localPort: "4096",
			relay:     "",
			want:      []string{"start -relay " + defaultRelay + " -port 4096"},
			notWant:   []string{"-relay  -port"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			unit := buildSystemdUnit(tc.localPort, tc.relay, tc.attach, "/usr/local/bin/tunnel-client")
			execStart := systemdExecStart(t, unit)
			for _, want := range tc.want {
				if !strings.Contains(execStart, want) {
					t.Fatalf("ExecStart = %q, want it to contain %q", execStart, want)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(execStart, notWant) {
					t.Fatalf("ExecStart = %q, must not contain %q", execStart, notWant)
				}
			}
		})
	}
}

func TestBuildSystemdUnitOmitsSecrets(t *testing.T) {
	unit := buildSystemdUnit("4096", "https://example.test", true, "/usr/local/bin/tunnel-client")
	lower := strings.ToLower(unit)
	for _, word := range []string{"passphrase", "password", "token", "secret"} {
		if strings.Contains(lower, word) {
			t.Fatalf("unit must not carry credentials, found %q:\n%s", word, unit)
		}
	}
}

// systemd 会对 ExecStart 做 specifier 展开与空白拆分，因此参数必须转义。
func TestSystemdEscapeArg(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain url unchanged",
			in:   "https://relay.example.test",
			want: "https://relay.example.test",
		},
		{
			name: "percent is doubled and quoted",
			in:   "https://relay.example.test/%2Fuser",
			want: `"https://relay.example.test/%%2Fuser"`,
		},
		{
			name: "space is quoted",
			in:   "https://relay.example.test/a b",
			want: `"https://relay.example.test/a b"`,
		},
		{
			name: "double quote is escaped",
			in:   `https://relay.example.test/a"b`,
			want: `"https://relay.example.test/a\"b"`,
		},
		{
			name: "backslash is escaped",
			in:   `C:\relay\tunnel`,
			want: `"C:\\relay\\tunnel"`,
		},
		{
			name: "empty is quoted",
			in:   "",
			want: `""`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := systemdEscapeArg(tc.in); got != tc.want {
				t.Fatalf("systemdEscapeArg(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildSystemdUnitEscapesRelay(t *testing.T) {
	unit := buildSystemdUnit("4096", "https://relay.example.test/%2Fuser", false, "/usr/local/bin/tunnel-client")
	execStart := systemdExecStart(t, unit)
	want := `-relay "https://relay.example.test/%%2Fuser"`
	if !strings.Contains(execStart, want) {
		t.Fatalf("ExecStart = %q, want it to contain %q", execStart, want)
	}
	if strings.Contains(execStart, `-relay https://relay.example.test/%2Fuser`) {
		t.Fatalf("ExecStart = %q, relay must not be left unescaped", execStart)
	}
}

func TestBuildSystemdUnitUsesExecPath(t *testing.T) {
	unit := buildSystemdUnit("4096", "https://example.test", false, "/opt/opencode tunnel/tunnel-client")
	execStart := systemdExecStart(t, unit)
	if !strings.HasPrefix(execStart, `"/opt/opencode tunnel/tunnel-client" `) {
		t.Fatalf("ExecStart = %q, want it to start with the escaped executable path", execStart)
	}
}

// setupSystemd 必须在无法解析可执行文件时失败，而不是写入一个开机必失败的 unit。
func TestSetupSystemdFailsWhenExecutableUnknown(t *testing.T) {
	old := osExecutable
	osExecutable = func() (string, error) { return "", errors.New("executable unavailable") }
	t.Cleanup(func() { osExecutable = old })

	if err := setupSystemd("4096", "https://example.test", false); err == nil {
		t.Fatal("setupSystemd() error = nil, want executable lookup error")
	}
}

// installAutoStart 必须把安装时解析出的 relay 与 --attach 透传给自启安装，
// 否则 unit 记录的行为与安装时不一致。
func TestInstallPassesRelayAndAttachToAutoStart(t *testing.T) {
	withTempConfigDir(t)
	if err := saveDeviceConfig(&DeviceConfig{
		DeviceName: "box", RelayURL: "https://relay.example", Subdomain: "s1", HostID: "s1",
		AuthUser: "u", AuthPassword: "p",
		KeyWrapped: "wrapped", KeySalt: "salt", KeyKdf: keyKdfScrypt,
	}); err != nil {
		t.Fatalf("saveDeviceConfig: %v", err)
	}

	var gotRelay string
	var gotAttach bool
	old := setupAutoStart
	setupAutoStart = func(localPort, relay string, attach bool) error {
		gotRelay, gotAttach = relay, attach
		return nil
	}
	t.Cleanup(func() { setupAutoStart = old })

	if err := installAutoStart(startOptions{Attach: true}, "https://relay.example", "box", "5000"); err != nil {
		t.Fatalf("installAutoStart: %v", err)
	}
	if gotRelay != "https://relay.example" {
		t.Fatalf("relay = %q, want https://relay.example", gotRelay)
	}
	if !gotAttach {
		t.Fatal("--attach must be passed through to the autostart installer")
	}
}

// 已有完整设备配置时也必须解析 relay：显式 -relay > device.RelayURL >
// auth.json RelayURL > defaultRelay。否则 unit 会把自托管地址写成上游默认值。
func TestInstallAutoStartResolvesRelayWithoutKeySetup(t *testing.T) {
	const (
		selfHosted = "https://relay.selfhosted.test"
		deviceURL  = "https://relay.device.test"
		explicit   = "https://relay.explicit.test"
	)

	tests := []struct {
		name      string
		deviceURL string
		authURL   string
		relayArg  string
		want      string
	}{
		{
			name:      "auth.json wins when device has no relay",
			deviceURL: "",
			authURL:   selfHosted,
			relayArg:  defaultRelay,
			want:      selfHosted,
		},
		{
			name:      "device relay wins over auth.json",
			deviceURL: deviceURL,
			authURL:   selfHosted,
			relayArg:  defaultRelay,
			want:      deviceURL,
		},
		{
			name:      "explicit relay wins over both",
			deviceURL: deviceURL,
			authURL:   selfHosted,
			relayArg:  explicit,
			want:      explicit,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withTempConfigDir(t)
			if err := saveDeviceConfig(&DeviceConfig{
				DeviceName: "box", RelayURL: tc.deviceURL, Subdomain: "s1", HostID: "s1",
				AuthUser: "u", AuthPassword: "p",
				KeyWrapped: "wrapped", KeySalt: "salt", KeyKdf: keyKdfScrypt,
			}); err != nil {
				t.Fatalf("saveDeviceConfig: %v", err)
			}
			if err := saveAuthConfig(&AuthConfig{RelayURL: tc.authURL, Token: "tok"}); err != nil {
				t.Fatalf("saveAuthConfig: %v", err)
			}

			var gotRelay string
			old := setupAutoStart
			setupAutoStart = func(localPort, relay string, attach bool) error {
				gotRelay = relay
				return nil
			}
			t.Cleanup(func() { setupAutoStart = old })

			if err := installAutoStart(startOptions{}, tc.relayArg, "box", "5000"); err != nil {
				t.Fatalf("installAutoStart: %v", err)
			}
			if gotRelay != tc.want {
				t.Fatalf("relay = %q, want %q", gotRelay, tc.want)
			}
			if gotRelay == defaultRelay && tc.want != defaultRelay {
				t.Fatalf("unit would point at %q instead of %q", defaultRelay, tc.want)
			}
		})
	}
}
