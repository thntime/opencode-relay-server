package services_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zero469/opencode-relay-server/internal/config"
	"github.com/zero469/opencode-relay-server/internal/services"
)

func registerTestUser(t *testing.T, db interface {
	CreateVerificationCode(email, code string, expiresAt time.Time) error
}, auth *services.AuthService, email, password string) *services.AuthService {
	t.Helper()
	code := "123456"
	db.CreateVerificationCode(email, code, time.Now().Add(10*time.Minute))
	auth.Register(email, password, code)
	return auth
}

func TestDeviceService_Register(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	cfg := &config.Config{
		FrpsHost:  "test.example.com",
		FrpsPort:  "7000",
		FrpsToken: "test-token",
		Domain:    "test.dpdns.org",
	}

	auth := services.NewAuthService(db, "test-secret", false)
	deviceSvc := services.NewDeviceService(db, cfg)

	createTestVerificationCode(t, db, "test@example.com", "123456")
	user, _ := auth.Register("test@example.com", "password123", "123456")

	tests := []struct {
		name       string
		userID     int64
		deviceName string
		wantErr    bool
	}{
		{
			name:       "successful device registration",
			userID:     user.ID,
			deviceName: "MacBook Pro",
			wantErr:    false,
		},
		{
			name:       "register another device",
			userID:     user.ID,
			deviceName: "iMac",
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			device, err := deviceSvc.Register(tt.userID, tt.deviceName)

			if tt.wantErr {
				if err == nil {
					t.Error("Register() expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("Register() unexpected error = %v", err)
				return
			}

			if device.Name != tt.deviceName {
				t.Errorf("Register() name = %v, want %v", device.Name, tt.deviceName)
			}

			if len(device.Subdomain) != 32 {
				t.Errorf("Register() subdomain length = %d, want 32", len(device.Subdomain))
			}

			if device.UserID != tt.userID {
				t.Errorf("Register() userID = %v, want %v", device.UserID, tt.userID)
			}
		})
	}
}

func TestDeviceService_List(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	cfg := &config.Config{}
	auth := services.NewAuthService(db, "test-secret", false)
	deviceSvc := services.NewDeviceService(db, cfg)

	createTestVerificationCode(t, db, "user1@example.com", "111111")
	createTestVerificationCode(t, db, "user2@example.com", "222222")
	user1, _ := auth.Register("user1@example.com", "password123", "111111")
	user2, _ := auth.Register("user2@example.com", "password123", "222222")

	deviceSvc.Register(user1.ID, "Device1")
	deviceSvc.Register(user1.ID, "Device2")
	deviceSvc.Register(user2.ID, "Device3")

	devices, err := deviceSvc.List(user1.ID)
	if err != nil {
		t.Fatalf("List() unexpected error = %v", err)
	}

	if len(devices) != 2 {
		t.Errorf("List() returned %d devices, want 2", len(devices))
	}

	devices2, err := deviceSvc.List(user2.ID)
	if err != nil {
		t.Fatalf("List() unexpected error = %v", err)
	}

	if len(devices2) != 1 {
		t.Errorf("List() returned %d devices, want 1", len(devices2))
	}
}

func TestDeviceService_Get(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	cfg := &config.Config{}
	auth := services.NewAuthService(db, "test-secret", false)
	deviceSvc := services.NewDeviceService(db, cfg)

	createTestVerificationCode(t, db, "user1@example.com", "111111")
	createTestVerificationCode(t, db, "user2@example.com", "222222")
	user1, _ := auth.Register("user1@example.com", "password123", "111111")
	user2, _ := auth.Register("user2@example.com", "password123", "222222")

	device, _ := deviceSvc.Register(user1.ID, "Device1")

	t.Run("owner can get device", func(t *testing.T) {
		got, err := deviceSvc.Get(user1.ID, device.ID)
		if err != nil {
			t.Errorf("Get() unexpected error = %v", err)
		}
		if got.ID != device.ID {
			t.Errorf("Get() device ID = %v, want %v", got.ID, device.ID)
		}
	})

	t.Run("non-owner cannot get device", func(t *testing.T) {
		_, err := deviceSvc.Get(user2.ID, device.ID)
		if err == nil {
			t.Error("Get() expected error for non-owner, got nil")
		}
	})
}

func TestDeviceService_Update(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	cfg := &config.Config{}
	auth := services.NewAuthService(db, "test-secret", false)
	deviceSvc := services.NewDeviceService(db, cfg)

	createTestVerificationCode(t, db, "test@example.com", "123456")
	user, _ := auth.Register("test@example.com", "password123", "123456")
	device, _ := deviceSvc.Register(user.ID, "OldName")

	updated, err := deviceSvc.Update(user.ID, device.ID, "NewName")
	if err != nil {
		t.Fatalf("Update() unexpected error = %v", err)
	}

	if updated.Name != "NewName" {
		t.Errorf("Update() name = %v, want NewName", updated.Name)
	}

	if updated.Subdomain != device.Subdomain {
		t.Error("Update() should not change subdomain")
	}
}

func TestDeviceService_Delete(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	cfg := &config.Config{}
	auth := services.NewAuthService(db, "test-secret", false)
	deviceSvc := services.NewDeviceService(db, cfg)

	createTestVerificationCode(t, db, "test@example.com", "123456")
	user, _ := auth.Register("test@example.com", "password123", "123456")
	device, _ := deviceSvc.Register(user.ID, "Device1")

	err := deviceSvc.Delete(user.ID, device.ID)
	if err != nil {
		t.Fatalf("Delete() unexpected error = %v", err)
	}

	_, err = deviceSvc.Get(user.ID, device.ID)
	if err == nil {
		t.Error("Get() should return error after delete")
	}
}

func TestDeviceService_Heartbeat(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	cfg := &config.Config{}
	auth := services.NewAuthService(db, "test-secret", false)
	deviceSvc := services.NewDeviceService(db, cfg)

	createTestVerificationCode(t, db, "test@example.com", "123456")
	user, _ := auth.Register("test@example.com", "password123", "123456")
	device, _ := deviceSvc.Register(user.ID, "Device1")

	err := deviceSvc.Heartbeat(device.Subdomain)
	if err != nil {
		t.Fatalf("Heartbeat() unexpected error = %v", err)
	}

	updated, _ := deviceSvc.Get(user.ID, device.ID)
	if !updated.Online {
		t.Error("Heartbeat() should set device online")
	}

	if updated.LastSeen.IsZero() {
		t.Error("Heartbeat() should update last_seen")
	}
}

func TestDeviceService_GetFrpcConfig(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	cfg := &config.Config{
		FrpsHost:  "frps.example.com",
		FrpsPort:  "7000",
		FrpsToken: "secret-token",
		Domain:    "test.dpdns.org",
	}

	auth := services.NewAuthService(db, "test-secret", false)
	deviceSvc := services.NewDeviceService(db, cfg)

	createTestVerificationCode(t, db, "test@example.com", "123456")
	user, _ := auth.Register("test@example.com", "password123", "123456")
	device, _ := deviceSvc.Register(user.ID, "Device1")

	frpcConfig, err := deviceSvc.GetFrpcConfig(user.ID, device.ID, "4096")
	if err != nil {
		t.Fatalf("GetFrpcConfig() unexpected error = %v", err)
	}

	if frpcConfig.ServerAddr != "frps.example.com" {
		t.Errorf("GetFrpcConfig() ServerAddr = %v, want frps.example.com", frpcConfig.ServerAddr)
	}

	if frpcConfig.ServerPort != "7000" {
		t.Errorf("GetFrpcConfig() ServerPort = %v, want 7000", frpcConfig.ServerPort)
	}

	if frpcConfig.Token != "secret-token" {
		t.Errorf("GetFrpcConfig() Token = %v, want secret-token", frpcConfig.Token)
	}

	if frpcConfig.Subdomain != device.Subdomain {
		t.Errorf("GetFrpcConfig() Subdomain = %v, want %v", frpcConfig.Subdomain, device.Subdomain)
	}

	if frpcConfig.LocalPort != "4096" {
		t.Errorf("GetFrpcConfig() LocalPort = %v, want 4096", frpcConfig.LocalPort)
	}
}

func TestDeviceService_GetFrpcConfig_MissingConfig(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	auth := services.NewAuthService(db, "test-secret", false)
	createTestVerificationCode(t, db, "frpc-missing@example.com", "654321")
	user, err := auth.Register("frpc-missing@example.com", "password123", "654321")
	if err != nil {
		t.Fatalf("failed to register test user: %v", err)
	}

	tests := []struct {
		name        string
		cfg         *config.Config
		wantMissing []string
	}{
		{
			name:        "all frps fields missing",
			cfg:         &config.Config{},
			wantMissing: []string{"FRPS_HOST", "FRPS_PORT", "FRPS_DOMAIN"},
		},
		{
			name:        "only host missing",
			cfg:         &config.Config{FrpsPort: "7000", Domain: "example.com"},
			wantMissing: []string{"FRPS_HOST"},
		},
		{
			name:        "only port missing",
			cfg:         &config.Config{FrpsHost: "frps.example.com", Domain: "example.com"},
			wantMissing: []string{"FRPS_PORT"},
		},
		{
			name:        "only domain missing",
			cfg:         &config.Config{FrpsHost: "frps.example.com", FrpsPort: "7000"},
			wantMissing: []string{"FRPS_DOMAIN"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deviceSvc := services.NewDeviceService(db, tt.cfg)
			device, err := deviceSvc.Register(user.ID, "Host")
			if err != nil {
				t.Fatalf("Register() unexpected error = %v", err)
			}

			got, err := deviceSvc.GetFrpcConfig(user.ID, device.ID, "4096")
			if err == nil {
				t.Fatalf("GetFrpcConfig() expected error, got config %+v", got)
			}
			if !errors.Is(err, services.ErrFrpsConfigMissing) {
				t.Errorf("GetFrpcConfig() error = %v, want errors.Is(..., ErrFrpsConfigMissing)", err)
			}
			for _, name := range tt.wantMissing {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("GetFrpcConfig() error = %q, want it to name %s", err.Error(), name)
				}
			}
		})
	}
}
