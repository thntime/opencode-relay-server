package services

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/zero469/opencode-relay-server/internal/models"
)

// ResolveHost looks up a host by its public ID (device subdomain) and checks
// that it belongs to the given user.
func (s *DeviceService) ResolveHost(userID int64, hostID string) (*models.Device, error) {
	device, err := s.db.GetDeviceBySubdomain(hostID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrHostNotFound
		}
		return nil, fmt.Errorf("resolve host: %w", err)
	}
	if device == nil {
		return nil, ErrHostNotFound
	}
	if device.UserID != userID {
		return nil, ErrHostForbidden
	}
	return device, nil
}

func (s *DeviceService) GetKeyMaterial(deviceID int64) (keyWrapped, keySalt, keyKdf string, err error) {
	return s.db.GetDeviceKeyMaterial(deviceID)
}

func (s *DeviceService) SetKeyMaterial(deviceID int64, keyWrapped, keySalt, keyKdf string) error {
	return s.db.SetDeviceKeyMaterial(deviceID, keyWrapped, keySalt, keyKdf)
}
