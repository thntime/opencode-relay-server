package database

import (
	"database/sql"
	"time"

	"github.com/zero469/opencode-relay-server/internal/models"
)

func (db *DB) CreateUser(email, passwordHash string) (*models.User, error) {
	result, err := db.Exec(
		"INSERT INTO users (email, password_hash) VALUES (?, ?)",
		email, passwordHash,
	)
	if err != nil {
		return nil, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}

	return db.GetUserByID(id)
}

func (db *DB) GetUserByID(id int64) (*models.User, error) {
	user := &models.User{}
	err := db.QueryRow(
		"SELECT id, email, password_hash, created_at, updated_at FROM users WHERE id = ?",
		id,
	).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return user, nil
}

func (db *DB) GetUserByEmail(email string) (*models.User, error) {
	user := &models.User{}
	err := db.QueryRow(
		"SELECT id, email, password_hash, created_at, updated_at FROM users WHERE email = ?",
		email,
	).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return user, nil
}

func (db *DB) CreateDevice(userID int64, name, subdomain, authUser, authPassword string) (*models.Device, error) {
	result, err := db.Exec(
		"INSERT INTO devices (user_id, name, subdomain, auth_user, auth_password) VALUES (?, ?, ?, ?, ?)",
		userID, name, subdomain, authUser, authPassword,
	)
	if err != nil {
		return nil, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}

	return db.GetDeviceByID(id)
}

func (db *DB) GetDeviceByID(id int64) (*models.Device, error) {
	device := &models.Device{}
	var lastSeen sql.NullTime
	err := db.QueryRow(
		"SELECT id, user_id, name, subdomain, auth_user, auth_password, key_wrapped, key_salt, key_kdf, online, last_seen, created_at, updated_at FROM devices WHERE id = ?",
		id,
	).Scan(&device.ID, &device.UserID, &device.Name, &device.Subdomain, &device.AuthUser, &device.AuthPassword, &device.KeyWrapped, &device.KeySalt, &device.KeyKdf, &device.Online, &lastSeen, &device.CreatedAt, &device.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if lastSeen.Valid {
		device.LastSeen = lastSeen.Time
	}
	return device, nil
}

func (db *DB) GetDeviceBySubdomain(subdomain string) (*models.Device, error) {
	device := &models.Device{}
	var lastSeen sql.NullTime
	err := db.QueryRow(
		"SELECT id, user_id, name, subdomain, auth_user, auth_password, key_wrapped, key_salt, key_kdf, online, last_seen, created_at, updated_at FROM devices WHERE subdomain = ?",
		subdomain,
	).Scan(&device.ID, &device.UserID, &device.Name, &device.Subdomain, &device.AuthUser, &device.AuthPassword, &device.KeyWrapped, &device.KeySalt, &device.KeyKdf, &device.Online, &lastSeen, &device.CreatedAt, &device.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if lastSeen.Valid {
		device.LastSeen = lastSeen.Time
	}
	return device, nil
}

func (db *DB) GetDevicesByUserID(userID int64) ([]*models.Device, error) {
	rows, err := db.Query(
		"SELECT id, user_id, name, subdomain, auth_user, auth_password, key_wrapped, key_salt, key_kdf, online, last_seen, created_at, updated_at FROM devices WHERE user_id = ? ORDER BY created_at DESC",
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []*models.Device
	for rows.Next() {
		device := &models.Device{}
		var lastSeen sql.NullTime
		if err := rows.Scan(&device.ID, &device.UserID, &device.Name, &device.Subdomain, &device.AuthUser, &device.AuthPassword, &device.KeyWrapped, &device.KeySalt, &device.KeyKdf, &device.Online, &lastSeen, &device.CreatedAt, &device.UpdatedAt); err != nil {
			return nil, err
		}
		if lastSeen.Valid {
			device.LastSeen = lastSeen.Time
		}
		devices = append(devices, device)
	}
	return devices, nil
}

func (db *DB) UpdateDevice(id int64, name string) (*models.Device, error) {
	_, err := db.Exec(
		"UPDATE devices SET name = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		name, id,
	)
	if err != nil {
		return nil, err
	}
	return db.GetDeviceByID(id)
}

func (db *DB) DeleteDevice(id int64) error {
	_, err := db.Exec("DELETE FROM devices WHERE id = ?", id)
	return err
}

func (db *DB) UpdateDeviceHeartbeat(id int64) error {
	_, err := db.Exec(
		"UPDATE devices SET online = TRUE, last_seen = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		time.Now(), id,
	)
	return err
}

func (db *DB) MarkOfflineDevices(timeout time.Duration) error {
	cutoff := time.Now().Add(-timeout)
	_, err := db.Exec(
		"UPDATE devices SET online = FALSE WHERE online = TRUE AND last_seen < ?",
		cutoff,
	)
	return err
}

func (db *DB) MarkDeviceOffline(id int64) error {
	_, err := db.Exec(
		"UPDATE devices SET online = FALSE, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		id,
	)
	return err
}

func (db *DB) MarkAllDevicesOffline() (int64, error) {
	result, err := db.Exec("UPDATE devices SET online = FALSE WHERE online = TRUE")
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (db *DB) SetDeviceKeyMaterial(deviceID int64, keyWrapped, keySalt, keyKdf string) error {
	_, err := db.Exec(
		"UPDATE devices SET key_wrapped = ?, key_salt = ?, key_kdf = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		keyWrapped, keySalt, keyKdf, deviceID,
	)
	return err
}

func (db *DB) GetDeviceKeyMaterial(deviceID int64) (string, string, string, error) {
	var keyWrapped, keySalt, keyKdf string
	err := db.QueryRow(
		"SELECT key_wrapped, key_salt, key_kdf FROM devices WHERE id = ?",
		deviceID,
	).Scan(&keyWrapped, &keySalt, &keyKdf)
	if err != nil {
		return "", "", "", err
	}
	return keyWrapped, keySalt, keyKdf, nil
}

func (db *DB) CreateVerificationCode(email, code string, expiresAt time.Time) error {
	_, err := db.Exec(
		"INSERT INTO verification_codes (email, code, expires_at) VALUES (?, ?, ?)",
		email, code, expiresAt,
	)
	return err
}

func (db *DB) GetValidVerificationCode(email, code string) (bool, error) {
	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM verification_codes WHERE email = ? AND code = ? AND expires_at > ? AND used = FALSE",
		email, code, time.Now(),
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (db *DB) MarkVerificationCodeUsed(email, code string) error {
	_, err := db.Exec(
		"UPDATE verification_codes SET used = TRUE WHERE email = ? AND code = ?",
		email, code,
	)
	return err
}

func (db *DB) DeleteExpiredVerificationCodes() error {
	_, err := db.Exec("DELETE FROM verification_codes WHERE expires_at < ?", time.Now())
	return err
}

func (db *DB) CreatePairingRequest(id string, userID int64, pairingCode string, expiresAt time.Time) (*models.PairingRequest, error) {
	_, err := db.Exec(
		"INSERT INTO pairing_requests (id, user_id, pairing_code, expires_at) VALUES (?, ?, ?, ?)",
		id, userID, pairingCode, expiresAt,
	)
	if err != nil {
		return nil, err
	}
	return db.GetPairingRequestByID(id)
}

func (db *DB) GetPairingRequestByID(id string) (*models.PairingRequest, error) {
	pr := &models.PairingRequest{}
	var deviceID sql.NullInt64
	err := db.QueryRow(
		"SELECT id, user_id, pairing_code, status, device_id, expires_at, created_at FROM pairing_requests WHERE id = ?",
		id,
	).Scan(&pr.ID, &pr.UserID, &pr.PairingCode, &pr.Status, &deviceID, &pr.ExpiresAt, &pr.CreatedAt)
	if err != nil {
		return nil, err
	}
	if deviceID.Valid {
		pr.DeviceID = &deviceID.Int64
	}
	return pr, nil
}

func (db *DB) CompletePairingRequest(id string, deviceID int64) error {
	_, err := db.Exec(
		"UPDATE pairing_requests SET status = 'completed', device_id = ? WHERE id = ?",
		deviceID, id,
	)
	return err
}

func (db *DB) DeleteExpiredPairingRequests() (int64, error) {
	result, err := db.Exec(
		"DELETE FROM pairing_requests WHERE expires_at < ? AND status = 'pending'",
		time.Now(),
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
