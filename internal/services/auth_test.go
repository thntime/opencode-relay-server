package services_test

import (
	"os"
	"testing"
	"time"

	"github.com/zero469/opencode-relay-server/internal/database"
	"github.com/zero469/opencode-relay-server/internal/services"
)

func setupTestDB(t *testing.T) database.Database {
	t.Helper()
	db, err := database.NewSQLite(":memory:")
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}
	return db
}

func createTestVerificationCode(t *testing.T, db database.Database, email, code string) {
	t.Helper()
	err := db.CreateVerificationCode(email, code, time.Now().Add(10*time.Minute))
	if err != nil {
		t.Fatalf("Failed to create verification code: %v", err)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

func TestAuthService_Register(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	auth := services.NewAuthService(db, "test-secret", false)

	tests := []struct {
		name      string
		email     string
		password  string
		code      string
		setupCode bool
		wantErr   error
		wantEmail string
	}{
		{
			name:      "successful registration",
			email:     "test@example.com",
			password:  "password123",
			code:      "123456",
			setupCode: true,
			wantErr:   nil,
			wantEmail: "test@example.com",
		},
		{
			name:      "invalid verification code",
			email:     "invalid@example.com",
			password:  "password123",
			code:      "000000",
			setupCode: false,
			wantErr:   services.ErrInvalidCode,
		},
		{
			name:      "duplicate email",
			email:     "test@example.com",
			password:  "password456",
			code:      "654321",
			setupCode: true,
			wantErr:   services.ErrEmailExists,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setupCode {
				createTestVerificationCode(t, db, tt.email, tt.code)
			}

			user, err := auth.Register(tt.email, tt.password, tt.code)

			if tt.wantErr != nil {
				if err != tt.wantErr {
					t.Errorf("Register() error = %v, wantErr %v", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Errorf("Register() unexpected error = %v", err)
				return
			}

			if user.Email != tt.wantEmail {
				t.Errorf("Register() email = %v, want %v", user.Email, tt.wantEmail)
			}

			if user.ID == 0 {
				t.Error("Register() user ID should not be 0")
			}
		})
	}
}

func TestAuthService_Login(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	auth := services.NewAuthService(db, "test-secret", false)

	createTestVerificationCode(t, db, "test@example.com", "123456")
	auth.Register("test@example.com", "password123", "123456")

	tests := []struct {
		name     string
		email    string
		password string
		wantErr  error
	}{
		{
			name:     "successful login",
			email:    "test@example.com",
			password: "password123",
			wantErr:  nil,
		},
		{
			name:     "wrong password",
			email:    "test@example.com",
			password: "wrongpassword",
			wantErr:  services.ErrInvalidCredentials,
		},
		{
			name:     "user not found",
			email:    "notfound@example.com",
			password: "password123",
			wantErr:  services.ErrInvalidCredentials,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, user, err := auth.Login(tt.email, tt.password)

			if tt.wantErr != nil {
				if err != tt.wantErr {
					t.Errorf("Login() error = %v, wantErr %v", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Errorf("Login() unexpected error = %v", err)
				return
			}

			if token == "" {
				t.Error("Login() token should not be empty")
			}

			if user.Email != tt.email {
				t.Errorf("Login() email = %v, want %v", user.Email, tt.email)
			}
		})
	}
}

func TestAuthService_ValidateToken(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	auth := services.NewAuthService(db, "test-secret", false)

	createTestVerificationCode(t, db, "test@example.com", "123456")
	user, _ := auth.Register("test@example.com", "password123", "123456")
	token, _, _ := auth.Login("test@example.com", "password123")

	tests := []struct {
		name       string
		token      string
		wantUserID int64
		wantErr    bool
	}{
		{
			name:       "valid token",
			token:      token,
			wantUserID: user.ID,
			wantErr:    false,
		},
		{
			name:       "invalid token",
			token:      "invalid-token",
			wantUserID: 0,
			wantErr:    true,
		},
		{
			name:       "empty token",
			token:      "",
			wantUserID: 0,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			userID, err := auth.ValidateToken(tt.token)

			if tt.wantErr {
				if err == nil {
					t.Error("ValidateToken() expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("ValidateToken() unexpected error = %v", err)
				return
			}

			if userID != tt.wantUserID {
				t.Errorf("ValidateToken() userID = %v, want %v", userID, tt.wantUserID)
			}
		})
	}
}

func TestGenerateSubdomain(t *testing.T) {
	subdomains := make(map[string]bool)

	for i := 0; i < 100; i++ {
		subdomain := services.GenerateSubdomain()

		if len(subdomain) != 32 {
			t.Errorf("GenerateSubdomain() length = %d, want 32", len(subdomain))
		}

		if subdomains[subdomain] {
			t.Errorf("GenerateSubdomain() generated duplicate: %s", subdomain)
		}
		subdomains[subdomain] = true
	}
}

func TestGenerateVerificationCode(t *testing.T) {
	codes := make(map[string]bool)

	for i := 0; i < 100; i++ {
		code := services.GenerateVerificationCode()

		if len(code) != 6 {
			t.Errorf("GenerateVerificationCode() length = %d, want 6", len(code))
		}

		for _, c := range code {
			if c < '0' || c > '9' {
				t.Errorf("GenerateVerificationCode() contains non-digit: %s", code)
			}
		}

		codes[code] = true
	}
}
