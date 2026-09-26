// Command influence is the entry point for the Influence documentation platform
// server. It runs as a single standalone process (Requirements 20.1, 20.2) and
// uses SQLite for all persistence with no separate database server (Requirement
// 20.3).
//
// main resolves layered configuration (task 2.1) and then runs the fixed
// fail-fast startup sequence (task 2.2): it validates the port, Data_Directory,
// TLS material, and database-file accessibility, and only binds the listener —
// and begins serving — once every check has passed. After a successful Prepare
// it opens the Central_Directory, builds the connection manager and the wired
// API server, and serves the real router (task 22.1).
package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"

	"os"

	"github.com/influence/influence/internal/api"
	"github.com/influence/influence/internal/config"
	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/server"
)

func main() {
	cfg, err := config.Resolve(os.Args[1:], os.LookupEnv)
	if err != nil {
		log.Fatalf("influence: configuration error: %v", err)
	}

	prepared, err := server.Prepare(context.Background(), cfg, server.Options{})
	if err != nil {
		// Prepare already carries a specific, class-based message per failure
		// (invalid port, missing/non-writable data dir, invalid TLS,
		// inaccessible database, port in use). Fail fast without serving.
		log.Fatalf("influence: startup failed: %v", err)
	}
	defer func() { _ = prepared.Listener.Close() }()

	// Open and migrate the Central_Directory, then build the per-tenant
	// connection manager over it. Prepare has already confirmed the file (if
	// present) is accessible; a first launch creates it here.
	central, err := sql.Open(data.DriverName, prepared.CentralPath)
	if err != nil {
		log.Fatalf("influence: open central directory: %v", err)
	}
	if _, err := central.Exec("PRAGMA foreign_keys = ON"); err != nil {
		log.Fatalf("influence: configure central directory: %v", err)
	}
	if err := data.MigrateCentral(context.Background(), central); err != nil {
		log.Fatalf("influence: migrate central directory: %v", err)
	}

	conns := data.NewConnManager(central)
	defer func() { _ = conns.Close() }()

	apiServer := api.NewServer(conns, cfg)
	handler := apiServer.Router()

	scheme := "http"
	if prepared.TLSConfig != nil {
		scheme = "https"
	}
	log.Printf("influence: serving on %s (%s), data dir %q", prepared.Listener.Addr(), scheme, cfg.DataDir)

	srv := &http.Server{Handler: handler}
	if err := srv.Serve(prepared.Listener); err != nil && err != http.ErrServerClosed {
		log.Fatalf("influence: serve error: %v", err)
	}
}
