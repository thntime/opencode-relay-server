package services_test

import (
	"errors"
	"testing"

	"github.com/zero469/opencode-relay-server/internal/config"
	"github.com/zero469/opencode-relay-server/internal/services"
)

func TestDeviceService_ResolveHost(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	auth := services.NewAuthService(db, "test-secret", false)
	deviceSvc := services.NewDeviceService(db, &config.Config{})

	createTestVerificationCode(t, db, "owner@example.com", "111111")
	createTestVerificationCode(t, db, "other@example.com", "222222")
	owner, _ := auth.Register("owner@example.com", "password123", "111111")
	other, _ := auth.Register("other@example.com", "password123", "222222")
	device, _ := deviceSvc.Register(owner.ID, "Host1")

	t.Run("owner resolves host", func(t *testing.T) {
		got, err := deviceSvc.ResolveHost(owner.ID, device.Subdomain)
		if err != nil {
			t.Fatalf("ResolveHost() unexpected error = %v", err)
		}
		if got.ID != device.ID {
			t.Errorf("ResolveHost() device ID = %v, want %v", got.ID, device.ID)
		}
	})

	t.Run("other user is forbidden", func(t *testing.T) {
		_, err := deviceSvc.ResolveHost(other.ID, device.Subdomain)
		if !errors.Is(err, services.ErrHostForbidden) {
			t.Errorf("ResolveHost() error = %v, want ErrHostForbidden", err)
		}
	})

	t.Run("unknown host is not found", func(t *testing.T) {
		_, err := deviceSvc.ResolveHost(owner.ID, "does-not-exist")
		if !errors.Is(err, services.ErrHostNotFound) {
			t.Errorf("ResolveHost() error = %v, want ErrHostNotFound", err)
		}
	})
}

func TestDeviceService_ResolveHost_DatabaseError(t *testing.T) {
	db := setupTestDB(t)
	deviceSvc := services.NewDeviceService(db, &config.Config{})

	if err := db.Close(); err != nil {
		t.Fatalf("failed to close database: %v", err)
	}

	_, err := deviceSvc.ResolveHost(1, "any-host")
	if err == nil {
		t.Fatal("ResolveHost() error = nil, want database error")
	}
	if errors.Is(err, services.ErrHostNotFound) || errors.Is(err, services.ErrHostForbidden) {
		t.Errorf("ResolveHost() error = %v, must not be ErrHostNotFound or ErrHostForbidden", err)
	}
}
