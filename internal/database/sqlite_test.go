package database_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zero469/opencode-relay-server/internal/database"
)

var storeTestSeq atomic.Int64

func uniqueSuffix() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), storeTestSeq.Add(1))
}

// runHostStoreTests is the shared assertion set for every Database backend.
func runHostStoreTests(t *testing.T, db database.Database) {
	t.Helper()

	user, err := db.CreateUser("host-test-"+uniqueSuffix()+"@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	subdomain := "host" + uniqueSuffix()
	device, err := db.CreateDevice(user.ID, "host-1", subdomain, "auth-user", "auth-pass")
	if err != nil {
		t.Fatalf("CreateDevice() error = %v", err)
	}

	bySubdomain, err := db.GetDeviceBySubdomain(subdomain)
	if err != nil {
		t.Fatalf("GetDeviceBySubdomain() error = %v", err)
	}
	if bySubdomain.ID != device.ID {
		t.Errorf("GetDeviceBySubdomain() ID = %d, want %d", bySubdomain.ID, device.ID)
	}

	wrapped, salt, kdf, err := db.GetDeviceKeyMaterial(device.ID)
	if err != nil {
		t.Fatalf("GetDeviceKeyMaterial() before set error = %v", err)
	}
	if wrapped != "" || salt != "" || kdf != "" {
		t.Errorf("GetDeviceKeyMaterial() before set = (%q, %q, %q), want empty", wrapped, salt, kdf)
	}

	if err := db.SetDeviceKeyMaterial(device.ID, "wrapped-key", "salt-value", "argon2id"); err != nil {
		t.Fatalf("SetDeviceKeyMaterial() error = %v", err)
	}

	wrapped, salt, kdf, err = db.GetDeviceKeyMaterial(device.ID)
	if err != nil {
		t.Fatalf("GetDeviceKeyMaterial() error = %v", err)
	}
	if wrapped != "wrapped-key" || salt != "salt-value" || kdf != "argon2id" {
		t.Errorf("GetDeviceKeyMaterial() = (%q, %q, %q), want (wrapped-key, salt-value, argon2id)", wrapped, salt, kdf)
	}

	bySubdomain, err = db.GetDeviceBySubdomain(subdomain)
	if err != nil {
		t.Fatalf("GetDeviceBySubdomain() after key set error = %v", err)
	}
	if bySubdomain.KeyWrapped != "wrapped-key" || bySubdomain.KeySalt != "salt-value" || bySubdomain.KeyKdf != "argon2id" {
		t.Errorf("GetDeviceBySubdomain() key material = (%q, %q, %q), want (wrapped-key, salt-value, argon2id)",
			bySubdomain.KeyWrapped, bySubdomain.KeySalt, bySubdomain.KeyKdf)
	}

	devices, err := db.GetDevicesByUserID(user.ID)
	if err != nil {
		t.Fatalf("GetDevicesByUserID() error = %v", err)
	}
	if len(devices) != 1 || devices[0].ID != device.ID {
		t.Errorf("GetDevicesByUserID() = %d devices, want exactly device %d", len(devices), device.ID)
	}
}

func TestSQLiteHostStore(t *testing.T) {
	db, err := database.NewSQLite(":memory:")
	if err != nil {
		t.Fatalf("NewSQLite() error = %v", err)
	}
	defer db.Close()

	runHostStoreTests(t, db)
}

func TestSQLiteMigratesLegacyDevicesTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db error = %v", err)
	}
	_, err = raw.Exec(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			email TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE devices (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			subdomain TEXT UNIQUE NOT NULL,
			auth_user TEXT NOT NULL DEFAULT '',
			auth_password TEXT NOT NULL DEFAULT '',
			online BOOLEAN DEFAULT FALSE,
			last_seen DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		t.Fatalf("create legacy schema error = %v", err)
	}
	raw.Close()

	db, err := database.NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite() on legacy db error = %v", err)
	}
	defer db.Close()

	runHostStoreTests(t, db)
}
