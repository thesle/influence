package server

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/influence/influence/internal/config"
	"github.com/influence/influence/internal/data"
)

// This file implements the fixed fail-fast startup sequence (task 2.2). Every
// validation runs in a defined order so that no partially-initialized server
// ever accepts traffic (design § "Startup order (fail-fast before serving)"):
//
//   1. validate the listen port is an integer in 1..65535, attempting no bind
//      on failure                                             (Req 29.3)
//   2. validate the Data_Directory exists and is writable     (Req 30.4, 30.5)
//   3. load and validate the TLS cert+key when TLS is enabled (Req 19.4)
//   4. confirm the Central_Directory and Tenant_Database files are accessible
//                                                             (Req 20.4)
//   5. bind the listener, failing fast if the port is in use  (Req 29.4)
//
// Only after all five steps pass does the caller begin serving requests, so
// every fail-fast check is enforced before any request is handled.

// PortMin and PortMax bound the valid TCP listen-port range (Req 29.1, 29.3).
const (
	PortMin = 1
	PortMax = 65535
)

// FailureClass identifies which startup validation step rejected the
// configuration. Each class maps to a specific, operator-facing error message
// so a failed launch tells the operator exactly what to fix.
type FailureClass string

const (
	// FailurePort — the listen port is not an integer in 1..65535 (Req 29.3).
	FailurePort FailureClass = "invalid_port"

	// FailureDataDirMissing — the Data_Directory does not exist (Req 30.4).
	FailureDataDirMissing FailureClass = "data_dir_missing"

	// FailureDataDirNotWritable — the Data_Directory exists but the process
	// cannot write to it (Req 30.5).
	FailureDataDirNotWritable FailureClass = "data_dir_not_writable"

	// FailureTLS — the SSL certificate/key is missing, unreadable, or
	// mismatched (Req 19.4).
	FailureTLS FailureClass = "invalid_tls"

	// FailureDatabaseInaccessible — a configured SQLite database file is
	// missing or inaccessible (Req 20.4).
	FailureDatabaseInaccessible FailureClass = "database_inaccessible"

	// FailurePortInUse — the (valid) listen port is already bound (Req 29.4).
	FailurePortInUse FailureClass = "port_in_use"
)

// StartupError carries the failure class plus a specific message. It wraps the
// underlying cause (when any) so callers can inspect it with errors.Is/As while
// still presenting a stable, class-specific message to the operator.
type StartupError struct {
	Class   FailureClass
	Message string
	Err     error
}

func (e *StartupError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *StartupError) Unwrap() error { return e.Err }

// newStartupError builds a StartupError for a class with a specific message and
// optional wrapped cause.
func newStartupError(class FailureClass, message string, cause error) *StartupError {
	return &StartupError{Class: class, Message: message, Err: cause}
}

// CentralDirectoryFileName is the Central_Directory database file inside the
// Data_Directory (design § Architecture; Req 30.3).
const CentralDirectoryFileName = "central_directory.db"

// ListenerBinder abstracts binding the network listener so the TLS variant
// (task 2.3) fills this seam without changing the ordered validation above. The
// default binder delegates to BuildListener (task 2.3, tls.go), which opens a
// plaintext TCP listener or a TLS-only listener depending on cfg.TLS. tlsCfg is
// passed through so a caller (or test) that has already built a validated
// *tls.Config can bind with it directly; when nil the binder builds the
// listener from cfg. Binding is the LAST startup step so a port-in-use failure
// is reported only after every other check has passed (Req 29.4).
type ListenerBinder func(cfg config.Config, tlsCfg *tls.Config) (net.Listener, error)

// defaultListenerBinder binds the listener for the resolved port. When a
// validated tlsCfg is supplied it binds a TLS listener with it directly;
// otherwise it delegates to BuildListener (task 2.3), which chooses plaintext
// vs TLS from cfg. Keeping the TLS mechanics in tls.go means 2.2's ordering is
// unaffected by how the TLS listener is constructed.
func defaultListenerBinder(cfg config.Config, tlsCfg *tls.Config) (net.Listener, error) {
	if tlsCfg != nil {
		addr := fmt.Sprintf(":%d", cfg.Port)
		return tls.Listen("tcp", addr, tlsCfg)
	}
	return BuildListener(cfg)
}

// PreparedServer is the outcome of a successful startup sequence: a bound
// listener ready to serve and the resolved paths that were validated. The
// caller (main) begins serving on Listener only after Prepare returns without
// error, guaranteeing every fail-fast check passed first.
type PreparedServer struct {
	// Listener is the bound network listener (plaintext or TLS). Serving starts
	// here; it is the caller's responsibility to Close it on shutdown.
	Listener net.Listener

	// CentralPath is the validated Central_Directory file path.
	CentralPath string

	// TenantPaths are the validated Tenant_Database file paths that existed at
	// startup and were confirmed accessible.
	TenantPaths []string

	// TLSConfig is the TLS configuration used for the listener, or nil when TLS
	// is disabled. Populated by the TLS seam (task 2.3).
	TLSConfig *tls.Config
}

// TLSConfigBuilder builds the *tls.Config for an SSL-enabled server from the
// resolved cert/key paths. This is the seam task 2.3 fills; when nil, Prepare
// uses buildValidatedTLSConfig, which validates the cert/key pair (Req 19.4)
// and sets MinVersion to TLS 1.2 as a safe default the TLS task can extend.
type TLSConfigBuilder func(cfg config.Config) (*tls.Config, error)

// Options tunes Prepare for testing and for the parallel TLS task. Zero-value
// Options uses the production defaults (real filesystem checks, real TCP bind).
type Options struct {
	// BindListener overrides how the listener is bound. Tests inject a binder
	// that avoids real sockets; task 2.3 injects a TLS-aware binder. Defaults
	// to defaultListenerBinder.
	BindListener ListenerBinder

	// BuildTLSConfig overrides TLS-config construction. Task 2.3 supplies the
	// production builder. Defaults to buildValidatedTLSConfig.
	BuildTLSConfig TLSConfigBuilder
}

// Prepare runs the fixed fail-fast startup sequence against cfg and returns a
// PreparedServer with a bound listener. It performs the five ordered checks and
// returns a *StartupError with a class-specific message on the first failure,
// having taken no later action (in particular, an invalid port yields no bind
// attempt — Req 29.3).
func Prepare(ctx context.Context, cfg config.Config, opts Options) (*PreparedServer, error) {
	if opts.BindListener == nil {
		opts.BindListener = defaultListenerBinder
	}
	if opts.BuildTLSConfig == nil {
		opts.BuildTLSConfig = buildValidatedTLSConfig
	}

	// Step 1: validate the port BEFORE anything else so an invalid port makes
	// no bind attempt (Req 29.3).
	if err := validatePort(cfg.Port); err != nil {
		return nil, err
	}

	// Step 2: validate the Data_Directory exists and is writable (Req 30.4,
	// 30.5).
	if err := validateDataDir(cfg.DataDir); err != nil {
		return nil, err
	}

	// Step 3: load/validate TLS cert+key when TLS is enabled (Req 19.4).
	var tlsCfg *tls.Config
	if cfg.TLS {
		built, err := opts.BuildTLSConfig(cfg)
		if err != nil {
			return nil, err
		}
		tlsCfg = built
	}

	// Step 4: confirm Central_Directory + Tenant_Database files are accessible
	// (Req 20.4).
	centralPath, tenantPaths, err := validateDatabaseFiles(ctx, cfg.DataDir)
	if err != nil {
		return nil, err
	}

	// Step 5: bind the listener LAST, failing fast if the port is in use
	// (Req 29.4). Only reached once every prior check passed.
	ln, err := opts.BindListener(cfg, tlsCfg)
	if err != nil {
		return nil, newStartupError(FailurePortInUse,
			fmt.Sprintf("listen port %d is unavailable (already in use)", cfg.Port), err)
	}

	return &PreparedServer{
		Listener:    ln,
		CentralPath: centralPath,
		TenantPaths: tenantPaths,
		TLSConfig:   tlsCfg,
	}, nil
}

// validatePort enforces the integer range 1..65535 (Req 29.1, 29.3). Because
// config.Resolve already parses the flag to an int (rejecting non-integers), a
// range violation here is the remaining "not a valid port" case; either way no
// bind is attempted.
func validatePort(port int) error {
	if port < PortMin || port > PortMax {
		return newStartupError(FailurePort,
			fmt.Sprintf("listen port %d is invalid: must be an integer in %d-%d", port, PortMin, PortMax), nil)
	}
	return nil
}

// validateDataDir checks that the Data_Directory exists (Req 30.4) and is
// writable by the running process (Req 30.5). Writability is probed by creating
// and removing a temporary file inside the directory, which reflects the
// process's effective permissions more reliably than inspecting mode bits.
func validateDataDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return newStartupError(FailureDataDirMissing,
				fmt.Sprintf("data directory %q does not exist", dir), nil)
		}
		return newStartupError(FailureDataDirMissing,
			fmt.Sprintf("data directory %q cannot be accessed", dir), err)
	}
	if !info.IsDir() {
		return newStartupError(FailureDataDirMissing,
			fmt.Sprintf("data directory %q is not a directory", dir), nil)
	}

	probe, err := os.CreateTemp(dir, ".influence-writable-*")
	if err != nil {
		return newStartupError(FailureDataDirNotWritable,
			fmt.Sprintf("data directory %q is not writable by the running process", dir), err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// validateDatabaseFiles confirms the Central_Directory and every existing
// Tenant_Database file inside the Data_Directory can be opened (Req 20.4).
//
// A first launch has no database files yet, so a not-yet-created
// Central_Directory is not a failure: absence means it will be created and
// migrated on first use. What Req 20.4 guards against is a file that is
// *present but inaccessible* — unreadable, or a valid path that SQLite cannot
// open. For any DB file that exists, this opens it and pings it; an open/ping
// failure aborts startup with the database-inaccessible message.
func validateDatabaseFiles(ctx context.Context, dir string) (centralPath string, tenantPaths []string, err error) {
	centralPath = filepath.Join(dir, CentralDirectoryFileName)

	if _, statErr := os.Stat(centralPath); statErr == nil {
		if openErr := checkDatabaseAccessible(ctx, centralPath); openErr != nil {
			return "", nil, newStartupError(FailureDatabaseInaccessible,
				fmt.Sprintf("central directory database %q cannot be accessed", centralPath), openErr)
		}
	} else if !os.IsNotExist(statErr) {
		return "", nil, newStartupError(FailureDatabaseInaccessible,
			fmt.Sprintf("central directory database %q cannot be accessed", centralPath), statErr)
	}

	// Tenant_Database files live alongside the Central_Directory as *.db files
	// (excluding the Central_Directory itself). Confirm each existing one is
	// accessible.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		return "", nil, newStartupError(FailureDatabaseInaccessible,
			fmt.Sprintf("data directory %q cannot be read to enumerate database files", dir), readErr)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == CentralDirectoryFileName {
			continue
		}
		if filepath.Ext(name) != ".db" {
			continue
		}
		path := filepath.Join(dir, name)
		if openErr := checkDatabaseAccessible(ctx, path); openErr != nil {
			return "", nil, newStartupError(FailureDatabaseInaccessible,
				fmt.Sprintf("tenant database %q cannot be accessed", path), openErr)
		}
		tenantPaths = append(tenantPaths, path)
	}

	return centralPath, tenantPaths, nil
}

// checkDatabaseAccessible opens a SQLite file with the registered driver and
// pings it to confirm it is a reachable, openable database. It uses the
// read-only-friendly path so a present file that cannot be opened surfaces as
// an error (Req 20.4). The handle is closed before returning.
func checkDatabaseAccessible(ctx context.Context, path string) error {
	// Reject a path that exists but is not readable up front; sql.Open is lazy
	// and would otherwise defer the error to Ping.
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	_ = f.Close()

	db, err := sql.Open(data.DriverName, path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		return err
	}
	// Touch the schema catalog so a corrupt/non-SQLite file is detected, not
	// just a reachable handle.
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&n); err != nil {
		return err
	}
	return nil
}

// buildValidatedTLSConfig is the default TLS-config builder used when TLS is
// enabled and no builder is injected. It delegates to TLSConfig (task 2.3,
// tls.go), which loads and validates the cert/key pair and pins MinVersion to
// TLS 1.2, then wraps any failure in the TLS failure class so a bad SSL
// configuration fails startup with a specific, operator-facing message
// (Req 19.4).
func buildValidatedTLSConfig(cfg config.Config) (*tls.Config, error) {
	tlsCfg, err := TLSConfig(cfg)
	if err != nil {
		return nil, newStartupError(FailureTLS,
			fmt.Sprintf("SSL configuration is invalid: certificate %q / key %q are missing, unreadable, or mismatched", cfg.TLSCert, cfg.TLSKey), err)
	}
	return tlsCfg, nil
}

// AsStartupError extracts a *StartupError from err, if present. It lets callers
// (and tests) branch on the failure class without string matching.
func AsStartupError(err error) (*StartupError, bool) {
	var se *StartupError
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}
