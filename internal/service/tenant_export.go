package service

// Tenant export (design § "Tenant export (Req 2)").
//
// A tenant's entire world — content, embedded image blobs, ciphertext, the
// Tenant_Salt and the wrapped DEK — lives inside one self-contained SQLite
// file (Req 2.1). Exporting a tenant for hand-off is therefore a consistent
// copy of that single file: because the file already carries its own
// TENANT_SECRET (Tenant_Salt + wrapped key material), the copy is decryptable
// on a successor host that holds the same server master secret, with no
// external content store or side-channel key delivery (Req 2.2, 16.9).
//
// The copy must be a consistent, non-corrupt snapshot even while the live
// database is being written. A naive byte-for-byte file copy of an open SQLite
// database can capture a torn write (a partially flushed page or an
// uncheckpointed WAL) and yield a corrupt export. Instead we use SQLite
// `VACUUM INTO`, which drives a transaction against the source and writes a
// fresh, defragmented, internally consistent database to the destination path
// — the same guarantee as the online-backup API for a whole-database copy.
//
// Failure handling (Req 2.4): if the source cannot be located (unknown/inactive
// tenant), is missing on disk, is already corrupt, or the snapshot cannot be
// written (e.g. the file is locked and the copy times out, or the destination
// is unwritable), ExportTenant aborts and returns an error; it does not leave a
// half-written export behind.
//
// PLACEMENT: this lives in the service layer alongside CreateTenant because it
// operates on the same TenantService state (the Central_Directory handle and
// the Data_Directory) and composes data-layer primitives; the data layer must
// not import service.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/influence/influence/internal/data"
)

// ErrExportDestinationRequired is returned when ExportTenant is called with an
// empty destination path.
var ErrExportDestinationRequired = errors.New("export destination path is required")

// ExportTenant produces a consistent, self-sufficient copy of a tenant's SQLite
// database at destPath (Req 2). The copy includes the tenant's TENANT_SECRET
// (Tenant_Salt + wrapped DEK), so a successor host holding the same server
// master secret can open the file and unwrap the DEK to decrypt stored
// ciphertext (Req 2.2, 16.9).
//
// It resolves the source db_path from the Central_Directory registry for the
// active tenant, verifies the source file exists and is not corrupt, and writes
// a snapshot via SQLite VACUUM INTO. If the tenant is unknown/inactive, the
// source is missing, locked, or corrupt, or the snapshot cannot be written,
// ExportTenant aborts with an error and does not leave a partial export behind
// (Req 2.4).
func (s *TenantService) ExportTenant(ctx context.Context, tenantID data.TenantID, destPath string) error {
	if destPath == "" {
		return ErrExportDestinationRequired
	}

	// Resolve the source path from the registry, restricted to active tenants
	// so a half-created (pending) tenant is never exported. An unknown or
	// inactive tenant yields ErrTenantNotFound, which the caller reports as an
	// export failure (Req 2.4).
	srcPath, err := s.activeTenantDBPath(ctx, tenantID)
	if err != nil {
		return err
	}

	// The source file must exist on disk. A registry row can outlive its file
	// if the file was moved or deleted out from under us; treat that as a
	// missing-source export failure (Req 2.4).
	if _, err := os.Stat(srcPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("export tenant %d: source database missing: %w", tenantID, err)
		}
		return fmt.Errorf("export tenant %d: stat source database: %w", tenantID, err)
	}

	// Open a short-lived handle to the source purely to drive the snapshot.
	src, err := sql.Open(data.DriverName, srcPath)
	if err != nil {
		return fmt.Errorf("export tenant %d: open source database: %w", tenantID, err)
	}
	defer func() { _ = src.Close() }()

	// Reject a corrupt source before attempting the snapshot (Req 2.4). A quick
	// integrity check surfaces on-disk corruption as an explicit error rather
	// than producing a copy of a broken database.
	if err := checkIntegrity(ctx, src); err != nil {
		return fmt.Errorf("export tenant %d: %w", tenantID, err)
	}

	// Never overwrite an existing destination silently — a stale file at
	// destPath could be mistaken for a fresh, valid export. VACUUM INTO itself
	// errors if the destination already exists, but checking first yields a
	// clearer message and avoids depending on that behavior.
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("export tenant %d: destination %q already exists", tenantID, destPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("export tenant %d: stat destination: %w", tenantID, err)
	}

	// Write a consistent, self-contained snapshot. VACUUM INTO runs in a
	// transaction against the source, so the copy captures a single point-in-
	// time image of every table — including tenant_secret — with no torn
	// writes. A locked source or an unwritable destination surfaces here as an
	// error; on failure we remove any partial destination file so no invalid
	// export is left behind (Req 2.4).
	if _, err := src.ExecContext(ctx, "VACUUM INTO ?", destPath); err != nil {
		_ = os.Remove(destPath)
		return fmt.Errorf("export tenant %d: snapshot database: %w", tenantID, err)
	}

	return nil
}

// activeTenantDBPath resolves the db_path for an active tenant from the
// Central_Directory registry. It mirrors the connection manager's resolution
// rule (status = 'active') so export can never target a pending, half-built
// tenant. An unknown or inactive tenant yields data.ErrTenantNotFound.
func (s *TenantService) activeTenantDBPath(ctx context.Context, tenantID data.TenantID) (string, error) {
	var path string
	err := s.central.QueryRowContext(ctx,
		`SELECT db_path FROM "TENANT" WHERE id = ? AND status = 'active'`, int64(tenantID),
	).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: tenant %d", data.ErrTenantNotFound, tenantID)
	}
	if err != nil {
		return "", fmt.Errorf("resolve tenant %d db path: %w", tenantID, err)
	}
	return path, nil
}

// checkIntegrity runs SQLite's quick integrity check against db and returns an
// error if the database is corrupt. A healthy database reports the single row
// "ok"; anything else (or a query error, e.g. the file is not a database)
// indicates corruption.
func checkIntegrity(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("source database is corrupt: %s", result)
	}
	return nil
}
