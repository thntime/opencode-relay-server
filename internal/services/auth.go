package services

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/zero469/opencode-relay-server/internal/database"
	"github.com/zero469/opencode-relay-server/internal/models"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrEmailExists        = errors.New("email already exists")
	ErrUserNotFound       = errors.New("user not found")
	ErrInvalidCode        = errors.New("invalid or expired verification code")
	ErrHostNotFound       = errors.New("host not found")
	ErrHostForbidden      = errors.New("host forbidden")
)

type AuthService struct {
	db             database.Database
	jwtSecret      []byte
	singleUserMode bool
}

func NewAuthService(db database.Database, jwtSecret string, singleUserMode bool) *AuthService {
	return &AuthService{
		db:             db,
		jwtSecret:      []byte(jwtSecret),
		singleUserMode: singleUserMode,
	}
}

func (s *AuthService) IsSingleUserMode() bool {
	return s.singleUserMode
}

func (s *AuthService) GetOrCreateSingleUser() (*models.User, error) {
	if !s.singleUserMode {
		return nil, errors.New("not in single user mode")
	}

	email := "admin@localhost"
	user, err := s.db.GetUserByEmail(email)
	if err == nil && user != nil {
		return user, nil
	}

	hash, _ := bcrypt.GenerateFromPassword([]byte("single-user"), bcrypt.DefaultCost)
	return s.db.CreateUser(email, string(hash))
}

// EnsureAdminUser creates an admin user if ADMIN_EMAIL and ADMIN_PASSWORD are set
// and the user doesn't already exist. This is idempotent and safe to call on every startup.
func (s *AuthService) EnsureAdminUser(email, password string) error {
	if email == "" || password == "" {
		return nil
	}

	existing, _ := s.db.GetUserByEmail(email)
	if existing != nil {
		return nil
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	_, err = s.db.CreateUser(email, string(hash))
	return err
}

func (s *AuthService) AutoLogin() (string, *models.User, error) {
	if !s.singleUserMode {
		return "", nil, errors.New("auto login only available in single user mode")
	}

	user, err := s.GetOrCreateSingleUser()
	if err != nil {
		return "", nil, err
	}

	token, err := s.generateToken(user.ID)
	if err != nil {
		return "", nil, err
	}

	return token, user, nil
}

func (s *AuthService) Register(email, password, code string) (*models.User, error) {
	valid, err := s.db.GetValidVerificationCode(email, code)
	if err != nil || !valid {
		return nil, ErrInvalidCode
	}

	existing, _ := s.db.GetUserByEmail(email)
	if existing != nil {
		return nil, ErrEmailExists
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	user, err := s.db.CreateUser(email, string(hash))
	if err != nil {
		return nil, err
	}

	s.db.MarkVerificationCodeUsed(email, code)
	return user, nil
}

func (s *AuthService) Login(email, password string) (string, *models.User, error) {
	user, err := s.db.GetUserByEmail(email)
	if err != nil {
		return "", nil, ErrInvalidCredentials
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return "", nil, ErrInvalidCredentials
	}

	token, err := s.generateToken(user.ID)
	if err != nil {
		return "", nil, err
	}

	return token, user, nil
}

func (s *AuthService) ValidateToken(tokenString string) (int64, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return s.jwtSecret, nil
	})
	if err != nil {
		return 0, err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return 0, errors.New("invalid token")
	}

	userID, ok := claims["user_id"].(float64)
	if !ok {
		return 0, errors.New("invalid token claims")
	}

	return int64(userID), nil
}

func (s *AuthService) generateToken(userID int64) (string, error) {
	claims := jwt.MapClaims{
		"user_id": userID,
		"exp":     time.Now().Add(30 * 24 * time.Hour).Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(s.jwtSecret)
}

func GenerateSubdomain() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

func GenerateRandomString(length int) string {
	bytes := make([]byte, length)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)[:length]
}

func GenerateVerificationCode() string {
	code := make([]byte, 3)
	rand.Read(code)
	num := int(code[0])<<16 | int(code[1])<<8 | int(code[2])
	return fmt.Sprintf("%06d", num%1000000)
}

func (s *AuthService) CreateVerificationCode(email string) (string, error) {
	code := GenerateVerificationCode()
	expiresAt := time.Now().Add(10 * time.Minute)
	err := s.db.CreateVerificationCode(email, code, expiresAt)
	if err != nil {
		return "", err
	}
	return code, nil
}
