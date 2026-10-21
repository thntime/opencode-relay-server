package models

import "time"

type User struct {
	ID           int64     `json:"id" bson:"id"`
	Email        string    `json:"email" bson:"email"`
	PasswordHash string    `json:"-" bson:"password_hash"`
	CreatedAt    time.Time `json:"created_at" bson:"created_at"`
	UpdatedAt    time.Time `json:"updated_at" bson:"updated_at"`
}

type Device struct {
	ID           int64     `json:"id" bson:"id"`
	UserID       int64     `json:"user_id" bson:"user_id"`
	Name         string    `json:"name" bson:"name"`
	Subdomain    string    `json:"subdomain" bson:"subdomain"`
	AuthUser     string    `json:"auth_user" bson:"auth_user"`
	AuthPassword string    `json:"auth_password" bson:"auth_password"`
	KeyWrapped   string    `json:"-" bson:"key_wrapped"`
	KeySalt      string    `json:"-" bson:"key_salt"`
	KeyKdf       string    `json:"-" bson:"key_kdf"`
	Online       bool      `json:"online" bson:"online"`
	LastSeen     time.Time `json:"last_seen" bson:"last_seen"`
	CreatedAt    time.Time `json:"created_at" bson:"created_at"`
	UpdatedAt    time.Time `json:"updated_at" bson:"updated_at"`
}

// HostDTO is the public view of a device as a persistent host.
// ID is Device.Subdomain.
type HostDTO struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Online   bool      `json:"online"`
	LastSeen time.Time `json:"last_seen"`
}

type SendVerificationRequest struct {
	Email string `json:"email"`
}

type RegisterRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type LoginResponse struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

type DeviceRegisterRequest struct {
	Name string `json:"name"`
}

type DeviceUpdateRequest struct {
	Name string `json:"name"`
}

type FrpcConfig struct {
	ServerAddr   string `json:"server_addr"`
	ServerPort   string `json:"server_port"`
	Token        string `json:"token"`
	Subdomain    string `json:"subdomain"`
	Domain       string `json:"domain"`
	LocalPort    string `json:"local_port"`
	AuthUser     string `json:"auth_user"`
	AuthPassword string `json:"auth_password"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

type MessageResponse struct {
	Message string `json:"message"`
}

type PairingRequest struct {
	ID          string    `json:"id" bson:"id"`
	UserID      int64     `json:"user_id" bson:"user_id"`
	PairingCode string    `json:"pairing_code" bson:"pairing_code"`
	Status      string    `json:"status" bson:"status"`
	DeviceID    *int64    `json:"device_id,omitempty" bson:"device_id,omitempty"`
	ExpiresAt   time.Time `json:"expires_at" bson:"expires_at"`
	CreatedAt   time.Time `json:"created_at" bson:"created_at"`
}

type CreatePairingResponse struct {
	ID          string    `json:"id"`
	PairingCode string    `json:"pairing_code"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type PairingStatusResponse struct {
	Status string  `json:"status"`
	Device *Device `json:"device,omitempty"`
}

type CompletePairingRequest struct {
	PairingCode string `json:"pairing_code"`
	DeviceName  string `json:"device_name"`
}

type VerificationCode struct {
	ID        int64     `json:"id" bson:"id"`
	Email     string    `json:"email" bson:"email"`
	Code      string    `json:"code" bson:"code"`
	ExpiresAt time.Time `json:"expires_at" bson:"expires_at"`
	Used      bool      `json:"used" bson:"used"`
	CreatedAt time.Time `json:"created_at" bson:"created_at"`
}
