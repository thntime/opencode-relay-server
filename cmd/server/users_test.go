package main

import (
	"bytes"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/zero469/opencode-relay-server/internal/database"
)

func newUsersTestDB(t *testing.T) database.Database {
	t.Helper()

	db, err := database.NewSQLite(":memory:")
	if err != nil {
		t.Fatalf("NewSQLite() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustCreateUser(t *testing.T, db database.Database, email, password string) int64 {
	t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword() error = %v", err)
	}
	user, err := db.CreateUser(email, string(hash))
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	return user.ID
}

func TestRunUsersCommandList(t *testing.T) {
	db := newUsersTestDB(t)
	if _, err := db.CreateUser("admin@example.com", "super-secret-hash"); err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	var out bytes.Buffer
	if err := runUsersCommand([]string{"list"}, db, &out); err != nil {
		t.Fatalf("runUsersCommand(list) error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "admin@example.com") {
		t.Errorf("list output = %q, want it to contain admin@example.com", got)
	}
	if strings.Contains(got, "super-secret-hash") {
		t.Errorf("list output leaked password hash: %q", got)
	}
}

func TestRunUsersCommandSetEmail(t *testing.T) {
	db := newUsersTestDB(t)
	userID := mustCreateUser(t, db, "old@example.com", "password")

	var out bytes.Buffer
	if err := runUsersCommand([]string{"set-email", "old@example.com", "new@example.com"}, db, &out); err != nil {
		t.Fatalf("runUsersCommand(set-email) error = %v", err)
	}

	updated, err := db.GetUserByEmail("new@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail(new) error = %v", err)
	}
	if updated.ID != userID {
		t.Errorf("updated user ID = %d, want %d", updated.ID, userID)
	}
	if _, err := db.GetUserByEmail("old@example.com"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetUserByEmail(old) error = %v, want sql.ErrNoRows", err)
	}
}

func TestRunUsersCommandSetEmailUnknownUser(t *testing.T) {
	db := newUsersTestDB(t)

	var out bytes.Buffer
	err := runUsersCommand([]string{"set-email", "missing@example.com", "new@example.com"}, db, &out)
	if err == nil {
		t.Fatal("runUsersCommand(set-email) error = nil, want error for unknown user")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to mention not found", err)
	}
}

func TestRunUsersCommandSetEmailConflict(t *testing.T) {
	db := newUsersTestDB(t)
	mustCreateUser(t, db, "first@example.com", "password")
	mustCreateUser(t, db, "second@example.com", "password")

	var out bytes.Buffer
	err := runUsersCommand([]string{"set-email", "first@example.com", "second@example.com"}, db, &out)
	if err == nil {
		t.Fatal("runUsersCommand(set-email) error = nil, want conflict error")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("error = %q, want it to mention already in use", err)
	}
}

func TestRunUsersCommandSetPassword(t *testing.T) {
	db := newUsersTestDB(t)
	userID := mustCreateUser(t, db, "user@example.com", "old-password")

	var out bytes.Buffer
	if err := runUsersCommand([]string{"set-password", "user@example.com", "--password", "new-password"}, db, &out); err != nil {
		t.Fatalf("runUsersCommand(set-password) error = %v", err)
	}

	updated, err := db.GetUserByID(userID)
	if err != nil {
		t.Fatalf("GetUserByID() error = %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(updated.PasswordHash), []byte("old-password")); err == nil {
		t.Error("old password still matches, want it to fail")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(updated.PasswordHash), []byte("new-password")); err != nil {
		t.Errorf("new password does not match: %v", err)
	}
}

func TestRunUsersCommandSetPasswordEmpty(t *testing.T) {
	db := newUsersTestDB(t)
	mustCreateUser(t, db, "user@example.com", "password")

	var out bytes.Buffer
	err := runUsersCommand([]string{"set-password", "user@example.com", "--password", ""}, db, &out)
	if err == nil {
		t.Fatal("runUsersCommand(set-password) error = nil, want error for empty password")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %q, want it to mention empty", err)
	}
}

func TestRunUsersCommandSetPasswordMismatch(t *testing.T) {
	db := newUsersTestDB(t)
	mustCreateUser(t, db, "user@example.com", "password")

	answers := []string{"first-password", "second-password"}
	original := readPassword
	readPassword = func() (string, error) {
		answer := answers[0]
		answers = answers[1:]
		return answer, nil
	}
	t.Cleanup(func() { readPassword = original })

	var out bytes.Buffer
	err := runUsersCommand([]string{"set-password", "user@example.com"}, db, &out)
	if err == nil {
		t.Fatal("runUsersCommand(set-password) error = nil, want mismatch error")
	}
	if !strings.Contains(err.Error(), "do not match") {
		t.Errorf("error = %q, want it to mention mismatch", err)
	}
}

func TestRunUsersCommandSetPasswordEqualsSyntax(t *testing.T) {
	db := newUsersTestDB(t)
	userID := mustCreateUser(t, db, "user@example.com", "old-password")

	var out bytes.Buffer
	if err := runUsersCommand([]string{"set-password", "user@example.com", "--password=new-password"}, db, &out); err != nil {
		t.Fatalf("runUsersCommand(set-password --password=value) error = %v", err)
	}

	updated, err := db.GetUserByID(userID)
	if err != nil {
		t.Fatalf("GetUserByID() error = %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(updated.PasswordHash), []byte("new-password")); err != nil {
		t.Errorf("new password does not match: %v", err)
	}
}

func TestRunUsersCommandSetPasswordReadError(t *testing.T) {
	db := newUsersTestDB(t)
	mustCreateUser(t, db, "user@example.com", "password")

	original := readPassword
	readPassword = func() (string, error) { return "", errors.New("terminal unavailable") }
	t.Cleanup(func() { readPassword = original })

	var out bytes.Buffer
	err := runUsersCommand([]string{"set-password", "user@example.com"}, db, &out)
	if err == nil {
		t.Fatal("runUsersCommand(set-password) error = nil, want read error")
	}
	if !strings.Contains(err.Error(), "read password") {
		t.Errorf("error = %q, want it to mention read password", err)
	}
}

func TestRunUsersCommandSetEmailSameEmailIsIdempotent(t *testing.T) {
	db := newUsersTestDB(t)
	userID := mustCreateUser(t, db, "same@example.com", "password")

	var out bytes.Buffer
	if err := runUsersCommand([]string{"set-email", "same@example.com", "same@example.com"}, db, &out); err != nil {
		t.Fatalf("runUsersCommand(set-email same) error = %v", err)
	}
	if !strings.Contains(out.String(), "already uses email") {
		t.Errorf("output = %q, want it to report the email is already in use", out.String())
	}

	user, err := db.GetUserByID(userID)
	if err != nil {
		t.Fatalf("GetUserByID() error = %v", err)
	}
	if user.Email != "same@example.com" {
		t.Errorf("email = %q, want same@example.com", user.Email)
	}
}

func TestRunUsersCommandSetPasswordUnknownUser(t *testing.T) {
	db := newUsersTestDB(t)

	var out bytes.Buffer
	err := runUsersCommand([]string{"set-password", "missing@example.com", "--password", "new-password"}, db, &out)
	if err == nil {
		t.Fatal("runUsersCommand(set-password) error = nil, want error for unknown user")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to mention not found", err)
	}
}

func TestRunUsersCommandUnknownSubcommand(t *testing.T) {
	db := newUsersTestDB(t)

	var out bytes.Buffer
	err := runUsersCommand([]string{"delete", "user@example.com"}, db, &out)
	if err == nil {
		t.Fatal("runUsersCommand(delete) error = nil, want unknown subcommand error")
	}
	if !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("error = %q, want it to mention unknown subcommand", err)
	}
}

func TestRunUsersCommandUsage(t *testing.T) {
	db := newUsersTestDB(t)

	var out bytes.Buffer
	if err := runUsersCommand(nil, db, &out); err == nil {
		t.Fatal("runUsersCommand(nil) error = nil, want usage error")
	}
	if err := runUsersCommand([]string{"set-email", "only-one"}, db, &out); err == nil {
		t.Fatal("runUsersCommand(set-email, one arg) error = nil, want usage error")
	}
	if err := runUsersCommand([]string{"set-password"}, db, &out); err == nil {
		t.Fatal("runUsersCommand(set-password, no args) error = nil, want usage error")
	}
	if err := runUsersCommand([]string{"set-password", "user@example.com", "--password"}, db, &out); err == nil {
		t.Fatal("runUsersCommand(set-password, missing value) error = nil, want usage error")
	}
	if err := runUsersCommand([]string{"set-password", "user@example.com", "--unknown"}, db, &out); err == nil {
		t.Fatal("runUsersCommand(set-password, unknown flag) error = nil, want usage error")
	}
}
