package database

import (
	"time"

	"github.com/zero469/opencode-relay-server/internal/models"
)

// Database defines the interface for all database operations
type Database interface {
	Close() error

	// User operations
	CreateUser(email, passwordHash string) (*models.User, error)
	GetUserByID(id int64) (*models.User, error)
	GetUserByEmail(email string) (*models.User, error)

	// Device operations
	CreateDevice(userID int64, name, subdomain, authUser, authPassword string) (*models.Device, error)
	GetDeviceByID(id int64) (*models.Device, error)
	GetDeviceBySubdomain(subdomain string) (*models.Device, error)
	GetDevicesByUserID(userID int64) ([]*models.Device, error)
	UpdateDevice(id int64, name string) (*models.Device, error)
	DeleteDevice(id int64) error
	UpdateDeviceHeartbeat(id int64) error
	MarkOfflineDevices(timeout time.Duration) error
	MarkDeviceOffline(id int64) error
	MarkAllDevicesOffline() (int64, error)

	// Host key material (end-to-end encryption)
	SetDeviceKeyMaterial(deviceID int64, keyWrapped, keySalt, keyKdf string) error
	GetDeviceKeyMaterial(deviceID int64) (keyWrapped, keySalt, keyKdf string, err error)

	// Verification code operations
	CreateVerificationCode(email, code string, expiresAt time.Time) error
	GetValidVerificationCode(email, code string) (bool, error)
	MarkVerificationCodeUsed(email, code string) error
	DeleteExpiredVerificationCodes() error

	// Pairing request operations
	CreatePairingRequest(id string, userID int64, pairingCode string, expiresAt time.Time) (*models.PairingRequest, error)
	GetPairingRequestByID(id string) (*models.PairingRequest, error)
	CompletePairingRequest(id string, deviceID int64) error
	DeleteExpiredPairingRequests() (int64, error)
}

// New creates a new database connection based on the provided URI.
// If uri starts with "mongodb://", it connects to MongoDB.
// Otherwise, it uses SQLite with the uri as the file path.
func New(uri string) (Database, error) {
	if len(uri) > 10 && uri[:10] == "mongodb://" {
		return NewMongoDB(uri)
	}
	if len(uri) > 14 && uri[:14] == "mongodb+srv://" {
		return NewMongoDB(uri)
	}
	return NewSQLite(uri)
}
