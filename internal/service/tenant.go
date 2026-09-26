// Influence — a self-hostable documentation platform.
// Copyright (C) 2026  Conrad Smith
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package service

// Transactional tenant creation (design § "Tenant creation (Req 1.7, 1.8)").
//
// Creating a tenant spans two independent stores that have no shared
// transaction: a row in the Central_Directory registry and a brand-new SQLite
// file on disk. There is no way to make an INSERT into Central_Directory and
// the creation+migration of a separate file atomic, so tenant creation is run
// as a saga with a compensating rollback:
//
//   1. Insert a `pending` row into the Central_Directory TENANT registry. The
//      row is deliberately `pending`, not `active`, so the connection manager —
//      which only resolves `active` tenants — never opens a half-built tenant
//      (see centralTenantRegistry in the data layer).
//   2. Create the new tenant SQLite file at a db_path inside the Data_Directory,
//      migrate it to the current tenant schema (data.MigrateTenant), and
//      initialize its per-tenant key material (InitTenantSecret with the server
//      master secret, task 12.1).
//   3. Flip the registry row to `active`. Only now is the tenant reachable.
//
// If anything in step 2 fails, the saga compensates: it deletes the pending
// registry row and removes any partial file that was created, then returns the
// error. The result is all-or-nothing — either a fully migrated tenant file
// with a matching `active` registry entry exists (Req 1.7), or NO trace of the
// tenant remains anywhere (Req 1.8). A `pending` row is never left behind for a
// tenant that failed to build.
//
// PLACEMENT: this orchestration lives in the service layer rather than the data
// layer on purpose. Step 2 needs InitTenantSecret, which lives in this package
// (crypto.go); the data layer must not import service (service already imports
// data), so putting the saga here keeps the dependency direction intact. It
// composes the data-layer primitives it needs — the Central_Directory handle
// via ConnManager.Central(), data.MigrateTenant, and the tenant SQLite driver —
// with the service-layer key provisioning.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// ErrTenantNameRequired is returned when CreateTenant is called with an empty
// tenant name. A tenant must be named so it can be identified in the registry.
var ErrTenantNameRequired = errors.New("tenant name is required")

// openTenantFile opens (creating if necessary) a tenant SQLite pool at path
// with foreign keys enabled, so the schema's ON DELETE CASCADE rules hold
// during and after migration. It is a package var so tests can force a
// file-creation/migration failure and exercise the rollback path.
var openTenantFile = func(path string) (*sql.DB, error) {
	db, err := sql.Open(data.DriverName, path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	return db, nil
}

// TenantService provisions new tenants transactionally. It holds the shared
// Central_Directory handle (via the connection manager), the Data_Directory
// where tenant files live, and the server master secret used to initialize each
// tenant's key material. It performs no per-request tenant routing; it only
// creates tenants.
type TenantService struct {
	central      *sql.DB
	dataDir      string
	masterSecret string
}

// NewTenantService constructs a TenantService over the connection manager's
// Central_Directory handle, the Data_Directory that holds tenant SQLite files,
// and the server master secret (resolved once at startup, Req 19.3). The master
// secret is used only to wrap each new tenant's DEK at creation time; it is
// never stored per-tenant.
func NewTenantService(conns data.ConnManager, dataDir, masterSecret string) *TenantService {
	return &TenantService{
		central:      conns.Central(),
		dataDir:      dataDir,
		masterSecret: masterSecret,
	}
}

// CreateTenant provisions a new tenant transactionally as a saga (Req 1.7,
// 1.8). On success it returns the new tenant's id, and a fully migrated tenant
// SQLite file with initialized key material exists alongside a matching
// `active` registry entry. On any failure it rolls back so no partial tenant
// remains: neither a registry row nor a file.
func (s *TenantService) CreateTenant(ctx context.Context, name string) (data.TenantID, error) {
	if name == "" {
		return 0, ErrTenantNameRequired
	}

	// Mint an external tenant identifier and derive its file path inside the
	// Data_Directory. Tenant files live alongside the Central_Directory as
	// tenant_<uuid>.db (see server startup's *.db discovery).
	tenantUUID := recordid.New().Canonical()
	dbPath := filepath.Join(s.dataDir, "tenant_"+tenantUUID+".db")

	// Step 1: insert the pending registry row. It is pending, not active, so the
	// connection manager cannot open the tenant until the saga completes.
	created := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.central.ExecContext(ctx,
		`INSERT INTO "TENANT" (tenant_uuid, name, db_path, status, created_at)
		 VALUES (?, ?, ?, 'pending', ?)`,
		tenantUUID, name, dbPath, created,
	)
	if err != nil {
		return 0, fmt.Errorf("insert pending tenant registry row: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		// The row was inserted but we cannot learn its id; compensate by
		// removing it so no orphaned pending row is left behind.
		s.rollback(ctx, tenantUUID, dbPath)
		return 0, fmt.Errorf("read new tenant id: %w", err)
	}

	// Step 2: build the tenant file. Any failure here triggers compensation.
	if err := s.buildTenantFile(ctx, dbPath); err != nil {
		s.rollback(ctx, tenantUUID, dbPath)
		return 0, fmt.Errorf("build tenant %q: %w", name, err)
	}

	// Step 3: flip the registry row to active. Only now is the tenant reachable
	// through the connection manager.
	if _, err := s.central.ExecContext(ctx,
		`UPDATE "TENANT" SET status = 'active' WHERE id = ?`, id,
	); err != nil {
		s.rollback(ctx, tenantUUID, dbPath)
		return 0, fmt.Errorf("activate tenant %q: %w", name, err)
	}

	return data.TenantID(id), nil
}

// buildTenantFile creates and migrates the tenant SQLite file and initializes
// its key material. Any error is returned to the caller, which compensates by
// rolling the whole saga back; this function does not clean up the file itself
// so rollback can remove it in one place.
func (s *TenantService) buildTenantFile(ctx context.Context, dbPath string) error {
	db, err := openTenantFile(dbPath)
	if err != nil {
		return fmt.Errorf("open tenant file: %w", err)
	}
	// Close the handle before returning so a later rollback can delete the file
	// without a lingering open descriptor.
	defer func() { _ = db.Close() }()

	if err := data.MigrateTenant(ctx, db); err != nil {
		return fmt.Errorf("migrate tenant file: %w", err)
	}

	if err := InitTenantSecret(ctx, db, s.masterSecret); err != nil {
		return fmt.Errorf("initialize tenant secret: %w", err)
	}
	return nil
}

// rollback compensates a failed saga: it removes the pending registry row and
// deletes any partial tenant file. Both steps are best-effort — the point is to
// leave no partial tenant (Req 1.8), and a compensation error should not mask
// the original failure the caller is about to return.
func (s *TenantService) rollback(ctx context.Context, tenantUUID, dbPath string) {
	// Remove the registry row. Scoping the delete to status='pending' ensures a
	// rollback can never remove an already-active tenant.
	_, _ = s.central.ExecContext(ctx,
		`DELETE FROM "TENANT" WHERE tenant_uuid = ? AND status = 'pending'`, tenantUUID,
	)
	// Remove any file that step 2 may have created. A missing file is fine.
	if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
		// Nothing actionable here; the registry row is gone, so the tenant is
		// unreachable regardless of a stray file. We intentionally do not
		// surface this over the original error.
		_ = err
	}
}
