package main

import (
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zero469/opencode-relay-server/internal/config"
	"github.com/zero469/opencode-relay-server/internal/database"
	"github.com/zero469/opencode-relay-server/internal/handlers"
	"github.com/zero469/opencode-relay-server/internal/middleware"
	"github.com/zero469/opencode-relay-server/internal/services"
	"github.com/zero469/opencode-relay-server/internal/tunnel"
)

func main() {
	cfg := config.Load()

	var db database.Database
	var err error

	mongoURI := os.Getenv("MONGODB_URI")
	if mongoURI != "" {
		log.Printf("Connecting to MongoDB...")
		db, err = database.NewMongoDB(mongoURI)
		if err != nil {
			log.Fatalf("Failed to connect to MongoDB: %v", err)
		}
		log.Printf("Connected to MongoDB successfully")
	} else {
		log.Printf("Using SQLite database: %s", cfg.DatabasePath)
		db, err = database.NewSQLite(cfg.DatabasePath)
		if err != nil {
			log.Fatalf("Failed to connect to SQLite: %v", err)
		}
	}
	defer db.Close()

	if count, err := db.MarkAllDevicesOffline(); err != nil {
		log.Printf("Warning: failed to mark devices offline on startup: %v", err)
	} else if count > 0 {
		log.Printf("Marked %d device(s) offline on server startup", count)
	}

	authService := services.NewAuthService(db, cfg.JWTSecret, cfg.SingleUserMode)

	if err := authService.EnsureAdminUser(cfg.AdminEmail, cfg.AdminPassword); err != nil {
		log.Printf("Warning: failed to create admin user: %v", err)
	} else if cfg.AdminEmail != "" {
		log.Printf("Admin user ensured: %s", cfg.AdminEmail)
	}

	deviceService := services.NewDeviceService(db, cfg)
	emailService := services.NewEmailService(cfg)
	pairingService := services.NewPairingService(db, cfg)

	tunnelManager := tunnel.NewManager()
	tunnelHandler := tunnel.NewHandler(tunnelManager, db, cfg.LegacyProxy)
	relayHandler := tunnel.NewRelayHandler(tunnelManager, deviceService)

	authHandler := handlers.NewAuthHandler(authService, emailService)
	deviceHandler := handlers.NewDeviceHandler(deviceService)
	hostHandler := handlers.NewHostHandler(deviceService)
	pairingHandler := handlers.NewPairingHandler(pairingService)

	authMiddleware := middleware.Auth(authService)

	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/send-verification", authHandler.SendVerification)
	mux.HandleFunc("POST /api/register", authHandler.Register)
	mux.HandleFunc("POST /api/login", authHandler.Login)
	mux.HandleFunc("POST /api/auto-login", authHandler.AutoLogin)

	mux.HandleFunc("GET /api/heartbeat", deviceHandler.Heartbeat)

	mux.Handle("POST /api/devices", authMiddleware(http.HandlerFunc(deviceHandler.Register)))
	mux.Handle("GET /api/devices", authMiddleware(http.HandlerFunc(deviceHandler.List)))
	mux.Handle("GET /api/devices/{id}", authMiddleware(http.HandlerFunc(deviceHandler.Get)))
	mux.Handle("PUT /api/devices/{id}", authMiddleware(http.HandlerFunc(deviceHandler.Update)))
	mux.Handle("DELETE /api/devices/{id}", authMiddleware(http.HandlerFunc(deviceHandler.Delete)))
	mux.Handle("GET /api/devices/{id}/frpc-config", authMiddleware(http.HandlerFunc(deviceHandler.GetFrpcConfig)))

	mux.Handle("GET /api/hosts", authMiddleware(http.HandlerFunc(hostHandler.List)))
	mux.Handle("GET /api/hosts/{hostId}/key", authMiddleware(http.HandlerFunc(hostHandler.GetKey)))
	mux.Handle("PUT /api/hosts/{hostId}/key", authMiddleware(http.HandlerFunc(hostHandler.PutKey)))

	mux.Handle("POST /api/pairing", authMiddleware(http.HandlerFunc(pairingHandler.Create)))
	mux.Handle("GET /api/pairing/{id}/status", authMiddleware(http.HandlerFunc(pairingHandler.GetStatus)))
	mux.Handle("POST /api/pairing/{id}/complete", authMiddleware(http.HandlerFunc(pairingHandler.Complete)))

	mux.HandleFunc("GET /api/tunnel/{subdomain}", tunnelHandler.HandleTunnelConnect)
	mux.HandleFunc("GET /api/events/{subdomain}", tunnelHandler.HandleEventConnect)
	mux.HandleFunc("POST /api/device/verify", tunnelHandler.HandleDeviceVerify)
	mux.Handle("/relay/", authMiddleware(http.HandlerFunc(relayHandler.HandleRelay)))
	mux.HandleFunc("/proxy/", tunnelHandler.HandleProxy)

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(indexHTML))
	})

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("GET /.well-known/apple-app-site-association", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
  "applinks": {
    "details": [
      {
        "appID": "9N465R4RJ8.com.opencode.anywhere",
        "paths": ["*"]
      }
    ]
  },
  "webcredentials": {
    "apps": ["9N465R4RJ8.com.opencode.anywhere"]
  }
}`))
	})

	mux.HandleFunc("GET /install.sh", handlers.ServeInstallScript)
	mux.HandleFunc("GET /install.ps1", handlers.ServeInstallScriptPS1)

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			db.MarkOfflineDevices(60 * time.Second)
			db.DeleteExpiredPairingRequests()
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		log.Printf("Received signal %v, shutting down...", sig)
		db.Close()
		os.Exit(0)
	}()

	handler := corsMiddleware(mux)

	log.Printf("Server starting on :%s", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, handler); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
