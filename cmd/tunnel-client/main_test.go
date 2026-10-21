package main

import (
	"bytes"
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
	setupAutoStart = func(localPort string) error {
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
