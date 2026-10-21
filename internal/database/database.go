package database

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
}

func NewSQLite(path string) (*DB, error) {
	var dsn string

	if path == ":memory:" {
		// In-memory database for tests - no pragmas needed
		dsn = path
	} else if strings.HasPrefix(path, "/data/") {
		// Azure Files SMB mount - use DELETE journal mode for compatibility
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
		// Use URI format with pragmas for SMB/Azure Files compatibility:
		// - busy_timeout: wait up to 10s when database is locked
		// - journal_mode=DELETE: rollback journal (WAL requires shared memory which SMB doesn't support)
		// - _txlock=immediate: acquire locks immediately to prevent deadlocks
		dsn = "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(DELETE)&_txlock=immediate"
	} else {
		// Local disk - use WAL journal mode for better performance
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
		// WAL mode provides better concurrency and performance on local disk
		dsn = "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	// Limit to single connection - critical for SQLite on network file systems
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		return nil, err
	}

	if err := migrate(db); err != nil {
		return nil, err
	}

	return &DB{db}, nil
}

func migrate(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		email TEXT UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS devices (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		name TEXT NOT NULL,
		subdomain TEXT UNIQUE NOT NULL,
		auth_user TEXT NOT NULL DEFAULT '',
		auth_password TEXT NOT NULL DEFAULT '',
		key_wrapped TEXT NOT NULL DEFAULT '',
		key_salt TEXT NOT NULL DEFAULT '',
		key_kdf TEXT NOT NULL DEFAULT '',
		online BOOLEAN DEFAULT FALSE,
		last_seen DATETIME,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_devices_user_id ON devices(user_id);
	CREATE INDEX IF NOT EXISTS idx_devices_subdomain ON devices(subdomain);

	CREATE TABLE IF NOT EXISTS verification_codes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		email TEXT NOT NULL,
		code TEXT NOT NULL,
		expires_at DATETIME NOT NULL,
		used BOOLEAN DEFAULT FALSE,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_verification_codes_email ON verification_codes(email);

	CREATE TABLE IF NOT EXISTS pairing_requests (
		id TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL,
		pairing_code TEXT NOT NULL,
		status TEXT DEFAULT 'pending',
		device_id INTEGER,
		expires_at DATETIME NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
		FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE SET NULL
	);
	CREATE INDEX IF NOT EXISTS idx_pairing_requests_user_id ON pairing_requests(user_id);
	`

	_, err := db.Exec(schema)
	if err != nil {
		return err
	}

	// Migration: Add auth columns if they don't exist (for existing databases)
	migrations := []string{
		`ALTER TABLE devices ADD COLUMN auth_user TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE devices ADD COLUMN auth_password TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE devices ADD COLUMN key_wrapped TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE devices ADD COLUMN key_salt TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE devices ADD COLUMN key_kdf TEXT NOT NULL DEFAULT ''`,
	}
	for _, m := range migrations {
		// Ignore errors (column may already exist)
		db.Exec(m)
	}

	return nil
}
