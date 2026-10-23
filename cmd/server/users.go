package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/zero469/opencode-relay-server/internal/database"
	"github.com/zero469/opencode-relay-server/internal/models"
)

const usersUsage = `usage:
  users list
  users set-email <old-email> <new-email>
  users set-password <email> [--password <password> | --password=<password>]`

// readPassword reads a password from the terminal without echoing it.
// It is a variable so tests can replace it.
var readPassword = func() (string, error) {
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	return string(raw), err
}

// runUsersCommand executes an administrative users subcommand against db.
// args excludes the leading "users" argument.
func runUsersCommand(args []string, db database.Database, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usersUsage)
	}

	switch args[0] {
	case "list":
		return runUsersList(args[1:], db, out)
	case "set-email":
		return runUsersSetEmail(args[1:], db, out)
	case "set-password":
		return runUsersSetPassword(args[1:], db, out)
	default:
		return fmt.Errorf("unknown subcommand %q\n%s", args[0], usersUsage)
	}
}

func runUsersList(args []string, db database.Database, out io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: users list")
	}

	users, err := db.ListUsers()
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	if len(users) == 0 {
		fmt.Fprintln(out, "No users found.")
		return nil
	}

	fmt.Fprintf(out, "%d user(s):\n", len(users))
	for _, user := range users {
		fmt.Fprintf(out, "%d\t%s\n", user.ID, user.Email)
	}
	return nil
}

func runUsersSetEmail(args []string, db database.Database, out io.Writer) error {
	if len(args) != 2 {
		return errors.New("usage: users set-email <old-email> <new-email>")
	}

	oldEmail := strings.TrimSpace(args[0])
	newEmail := strings.TrimSpace(args[1])
	if oldEmail == "" || newEmail == "" {
		return errors.New("email must not be empty")
	}

	user, err := findUserByEmail(db, oldEmail)
	if err != nil {
		return err
	}
	if oldEmail == newEmail {
		fmt.Fprintf(out, "User %d already uses email %s\n", user.ID, newEmail)
		return nil
	}

	existing, err := db.GetUserByEmail(newEmail)
	switch {
	case err == nil && existing != nil:
		return fmt.Errorf("email %q is already in use by user %d", newEmail, existing.ID)
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("check email %q: %w", newEmail, err)
	}

	if err := db.UpdateUserEmail(user.ID, newEmail); err != nil {
		if isEmailConflict(err) {
			return fmt.Errorf("email %q is already in use", newEmail)
		}
		return fmt.Errorf("update email for user %d: %w", user.ID, err)
	}

	fmt.Fprintf(out, "Updated email for user %d: %s -> %s\n", user.ID, oldEmail, newEmail)
	return nil
}

func runUsersSetPassword(args []string, db database.Database, out io.Writer) error {
	email, password, passwordSet, err := parseSetPasswordArgs(args)
	if err != nil {
		return err
	}

	if !passwordSet {
		fmt.Fprint(out, "New password: ")
		first, err := readPassword()
		fmt.Fprintln(out)
		if err != nil {
			return fmt.Errorf("read password: %w", err)
		}

		fmt.Fprint(out, "Confirm password: ")
		second, err := readPassword()
		fmt.Fprintln(out)
		if err != nil {
			return fmt.Errorf("read password: %w", err)
		}

		if first != second {
			return errors.New("passwords do not match")
		}
		password = first
	}

	if password == "" {
		return errors.New("password must not be empty")
	}

	user, err := findUserByEmail(db, email)
	if err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if err := db.UpdateUserPassword(user.ID, string(hash)); err != nil {
		return fmt.Errorf("update password for user %d: %w", user.ID, err)
	}

	fmt.Fprintf(out, "Updated password for user %d (%s)\n", user.ID, user.Email)
	return nil
}

func parseSetPasswordArgs(args []string) (email, password string, passwordSet bool, err error) {
	usage := errors.New("usage: users set-password <email> [--password <password> | --password=<password>]")

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--password":
			if i+1 >= len(args) {
				return "", "", false, errors.New("--password requires a value")
			}
			password = args[i+1]
			passwordSet = true
			i++
		case strings.HasPrefix(arg, "--password="):
			password = strings.TrimPrefix(arg, "--password=")
			passwordSet = true
		case strings.HasPrefix(arg, "-"):
			return "", "", false, fmt.Errorf("unknown flag %q", arg)
		default:
			if email != "" {
				return "", "", false, usage
			}
			email = strings.TrimSpace(arg)
		}
	}

	if email == "" {
		return "", "", false, usage
	}
	return email, password, passwordSet, nil
}

func findUserByEmail(db database.Database, email string) (*models.User, error) {
	user, err := db.GetUserByEmail(email)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("user %q not found", email)
		}
		return nil, fmt.Errorf("look up user %q: %w", email, err)
	}
	if user == nil {
		return nil, fmt.Errorf("user %q not found", email)
	}
	return user, nil
}

func isEmailConflict(err error) bool {
	if errors.Is(err, database.ErrUserEmailTaken) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint failed") ||
		strings.Contains(message, "duplicate key") ||
		strings.Contains(message, "e11000")
}
