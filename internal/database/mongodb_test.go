package database_test

import (
	"os"
	"testing"

	"github.com/zero469/opencode-relay-server/internal/database"
)

func TestMongoHostStore(t *testing.T) {
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		t.Skip("MONGODB_URI not set")
	}

	db, err := database.NewMongoDB(uri)
	if err != nil {
		t.Fatalf("NewMongoDB() error = %v", err)
	}
	defer db.Close()

	runHostStoreTests(t, db)
}
