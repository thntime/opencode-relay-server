package models_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zero469/opencode-relay-server/internal/models"
)

func TestHostDTOJSON(t *testing.T) {
	data, err := json.Marshal(models.HostDTO{
		ID:       "abc123",
		Name:     "my-host",
		Online:   true,
		LastSeen: time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	want := []string{"id", "name", "online", "last_seen"}
	if len(got) != len(want) {
		t.Errorf("HostDTO JSON keys = %v, want exactly %v", got, want)
	}
	for _, key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("HostDTO JSON missing key %q", key)
		}
	}
}

func TestDeviceJSONHidesSecrets(t *testing.T) {
	data, err := json.Marshal(models.Device{
		ID:           1,
		Name:         "host",
		Subdomain:    "abc",
		AuthUser:     "user",
		AuthPassword: "password",
		KeyWrapped:   "wrapped",
		KeySalt:      "salt",
		KeyKdf:       "kdf",
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	// 既有配对流程依赖 Device.auth_password 下发，凭据隔离由 HostDTO 保证
	out := string(data)
	for _, secret := range []string{"key_wrapped", "key_salt", "key_kdf", "wrapped", "salt", "kdf"} {
		if strings.Contains(out, secret) {
			t.Errorf("Device JSON leaks %q: %s", secret, out)
		}
	}
}
