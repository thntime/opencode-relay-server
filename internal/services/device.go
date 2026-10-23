package services

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zero469/opencode-relay-server/internal/config"
	"github.com/zero469/opencode-relay-server/internal/database"
	"github.com/zero469/opencode-relay-server/internal/models"
)

var ErrFrpsConfigMissing = errors.New("frps configuration is missing")

type DeviceService struct {
	db  database.Database
	cfg *config.Config
}

func NewDeviceService(db database.Database, cfg *config.Config) *DeviceService {
	return &DeviceService{db: db, cfg: cfg}
}

func (s *DeviceService) Register(userID int64, name string) (*models.Device, error) {
	subdomain := GenerateSubdomain()

	for {
		existing, _ := s.db.GetDeviceBySubdomain(subdomain)
		if existing == nil {
			break
		}
		subdomain = GenerateSubdomain()
	}

	authUser := GenerateRandomString(8)
	authPassword := GenerateRandomString(32)

	return s.db.CreateDevice(userID, name, subdomain, authUser, authPassword)
}

func (s *DeviceService) List(userID int64) ([]*models.Device, error) {
	return s.db.GetDevicesByUserID(userID)
}

func (s *DeviceService) Get(userID, deviceID int64) (*models.Device, error) {
	device, err := s.db.GetDeviceByID(deviceID)
	if err != nil {
		return nil, err
	}
	if device.UserID != userID {
		return nil, ErrUserNotFound
	}
	return device, nil
}

func (s *DeviceService) Update(userID, deviceID int64, name string) (*models.Device, error) {
	device, err := s.Get(userID, deviceID)
	if err != nil {
		return nil, err
	}
	return s.db.UpdateDevice(device.ID, name)
}

func (s *DeviceService) Delete(userID, deviceID int64) error {
	device, err := s.Get(userID, deviceID)
	if err != nil {
		return err
	}
	return s.db.DeleteDevice(device.ID)
}

func (s *DeviceService) Heartbeat(subdomain string) error {
	device, err := s.db.GetDeviceBySubdomain(subdomain)
	if err != nil {
		return err
	}
	return s.db.UpdateDeviceHeartbeat(device.ID)
}

func (s *DeviceService) GetFrpcConfig(userID, deviceID int64, localPort string) (*models.FrpcConfig, error) {
	device, err := s.Get(userID, deviceID)
	if err != nil {
		return nil, err
	}

	var missing []string
	if s.cfg.FrpsHost == "" {
		missing = append(missing, "FRPS_HOST")
	}
	if s.cfg.FrpsPort == "" {
		missing = append(missing, "FRPS_PORT")
	}
	if s.cfg.Domain == "" {
		missing = append(missing, "FRPS_DOMAIN")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrFrpsConfigMissing, strings.Join(missing, ", "))
	}

	return &models.FrpcConfig{
		ServerAddr:   s.cfg.FrpsHost,
		ServerPort:   s.cfg.FrpsPort,
		Token:        s.cfg.FrpsToken,
		Subdomain:    device.Subdomain,
		Domain:       s.cfg.Domain,
		LocalPort:    localPort,
		AuthUser:     device.AuthUser,
		AuthPassword: device.AuthPassword,
	}, nil
}
