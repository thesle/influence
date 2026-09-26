package server

import (
	"context"
	"crypto/tls"
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/influence/influence/internal/config"
	"github.com/influence/influence/internal/data"
)

// noBindBinder records whether the listener binder was ever invoked. Startup
// steps that fail before step 5 must never attempt a bind (Req 29.3 in
// particular).
type noBindBinder struct {
	called bool
}

func (b *noBindBinder) bind(cfg config.Config, tlsCfg *tls.Config) (net.Listener, error) {
	b.called = true
	// Return a real, immediately-closed pipe-free listener substitute: a
	// closed in-memory listener isn't available, so bind to an ephemeral port
	// and hand it back. Tests that expect success close it themselves.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	return ln, err
}

// writableDataDir returns a fresh, writable temp directory usable as the
// Data_Directory.
func writableDataDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func baseConfig(dir string) config.Config {
	return config.Config{
		Port:    8080,
		DataDir: dir,
	}
}

func TestPrepare_InvalidPortMakesNoBindAttempt(t *testing.T) {
	// Req 29.3: an out-of-range port fails to start and makes NO bind attempt.
	dir := writableDataDir(t)
	binder := &noBindBinder{}

	for _, port := range []int{0, -1, 65536, 100000} {
		binder.called = false
		cfg := baseConfig(dir)
		cfg.Port = port

		_, err := Prepare(context.Background(), cfg, Options{BindListener: binder.bind})
		if err == nil {
			t.Fatalf("port %d: expected startup error, got nil", port)
		}
		se, ok := AsStartupError(err)
		if !ok || se.Class != FailurePort {
			t.Fatalf("port %d: expected FailurePort, got %v", port, err)
		}
		if binder.called {
			t.Fatalf("port %d: binder was called; a bind must not be attempted for an invalid port", port)
		}
	}
}

func TestPrepare_MissingDataDir(t *testing.T) {
	// Req 30.4: a missing Data_Directory fails to start with a specific error.
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	binder := &noBindBinder{}

	_, err := Prepare(context.Background(), baseConfig(dir), Options{BindListener: binder.bind})
	se, ok := AsStartupError(err)
	if !ok || se.Class != FailureDataDirMissing {
		t.Fatalf("expected FailureDataDirMissing, got %v", err)
	}
	if binder.called {
		t.Fatal("binder must not be called when the data dir is missing")
	}
}

func TestPrepare_NonWritableDataDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: write-permission checks are bypassed by the OS")
	}
	// Req 30.5: an existing but non-writable Data_Directory fails to start.
	dir := writableDataDir(t)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	binder := &noBindBinder{}
	_, err := Prepare(context.Background(), baseConfig(dir), Options{BindListener: binder.bind})
	se, ok := AsStartupError(err)
	if !ok || se.Class != FailureDataDirNotWritable {
		t.Fatalf("expected FailureDataDirNotWritable, got %v", err)
	}
	if binder.called {
		t.Fatal("binder must not be called when the data dir is not writable")
	}
}

func TestPrepare_InvalidTLS(t *testing.T) {
	// Req 19.4: missing/mismatched TLS material fails to start.
	dir := writableDataDir(t)
	binder := &noBindBinder{}

	t.Run("missing paths", func(t *testing.T) {
		cfg := baseConfig(dir)
		cfg.TLS = true
		_, err := Prepare(context.Background(), cfg, Options{BindListener: binder.bind})
		se, ok := AsStartupError(err)
		if !ok || se.Class != FailureTLS {
			t.Fatalf("expected FailureTLS, got %v", err)
		}
	})

	t.Run("unreadable/nonexistent cert", func(t *testing.T) {
		cfg := baseConfig(dir)
		cfg.TLS = true
		cfg.TLSCert = filepath.Join(dir, "missing.crt")
		cfg.TLSKey = filepath.Join(dir, "missing.key")
		binder.called = false
		_, err := Prepare(context.Background(), cfg, Options{BindListener: binder.bind})
		se, ok := AsStartupError(err)
		if !ok || se.Class != FailureTLS {
			t.Fatalf("expected FailureTLS, got %v", err)
		}
		if binder.called {
			t.Fatal("binder must not be called on TLS failure")
		}
	})
}

func TestPrepare_InaccessibleDatabaseFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: read-permission checks are bypassed by the OS")
	}
	// Req 20.4: a present-but-inaccessible DB file fails to start.
	dir := writableDataDir(t)
	central := filepath.Join(dir, CentralDirectoryFileName)
	if err := os.WriteFile(central, []byte("not-a-db"), 0o000); err != nil {
		t.Fatalf("write central: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(central, 0o600) })

	binder := &noBindBinder{}
	_, err := Prepare(context.Background(), baseConfig(dir), Options{BindListener: binder.bind})
	se, ok := AsStartupError(err)
	if !ok || se.Class != FailureDatabaseInaccessible {
		t.Fatalf("expected FailureDatabaseInaccessible, got %v", err)
	}
	if binder.called {
		t.Fatal("binder must not be called when a database file is inaccessible")
	}
}

func TestPrepare_PortInUseFailsLast(t *testing.T) {
	// Req 29.4: a valid-but-unavailable port fails fast on bind, and only after
	// all earlier checks pass.
	dir := writableDataDir(t)

	failingBinder := func(cfg config.Config, tlsCfg *tls.Config) (net.Listener, error) {
		// Simulate "already in use" by returning a bind error.
		return nil, &net.OpError{Op: "listen", Err: net.UnknownNetworkError("address already in use")}
	}

	_, err := Prepare(context.Background(), baseConfig(dir), Options{BindListener: failingBinder})
	se, ok := AsStartupError(err)
	if !ok || se.Class != FailurePortInUse {
		t.Fatalf("expected FailurePortInUse, got %v", err)
	}
}

func TestPrepare_SuccessWithValidCentralDB(t *testing.T) {
	// Happy path: a valid, migrated Central_Directory + a migrated tenant DB,
	// a real bind on an ephemeral port. Prepare returns a bound listener.
	dir := writableDataDir(t)

	// Create and migrate a real Central_Directory so it is genuinely
	// accessible.
	central := filepath.Join(dir, CentralDirectoryFileName)
	mustMigrate(t, central, data.MigrateCentral)

	// Create and migrate a tenant DB alongside it.
	tenant := filepath.Join(dir, "tenant_a.db")
	mustMigrate(t, tenant, data.MigrateTenant)

	cfg := baseConfig(dir)
	// Use a valid configured port but bind on an ephemeral socket to avoid
	// collisions; validatePort still checks the configured value.
	ephemeralBinder := func(config.Config, *tls.Config) (net.Listener, error) {
		return net.Listen("tcp", "127.0.0.1:0")
	}

	prepared, err := Prepare(context.Background(), cfg, Options{BindListener: ephemeralBinder})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	defer prepared.Listener.Close()

	if prepared.CentralPath != central {
		t.Errorf("central path = %q, want %q", prepared.CentralPath, central)
	}
	if len(prepared.TenantPaths) != 1 || prepared.TenantPaths[0] != tenant {
		t.Errorf("tenant paths = %v, want [%q]", prepared.TenantPaths, tenant)
	}
	if prepared.TLSConfig != nil {
		t.Errorf("expected no TLS config when TLS disabled")
	}
}

func TestPrepare_SuccessOnFirstLaunchNoDBYet(t *testing.T) {
	// A first launch has no database files yet; that is not a failure.
	dir := writableDataDir(t)
	cfg := baseConfig(dir)
	ephemeralBinder := func(config.Config, *tls.Config) (net.Listener, error) {
		return net.Listen("tcp", "127.0.0.1:0")
	}

	prepared, err := Prepare(context.Background(), cfg, Options{BindListener: ephemeralBinder})
	if err != nil {
		t.Fatalf("expected success on empty data dir, got %v", err)
	}
	defer prepared.Listener.Close()

	if len(prepared.TenantPaths) != 0 {
		t.Errorf("expected no tenant paths on first launch, got %v", prepared.TenantPaths)
	}
}

func TestPrepare_OrderTLSBeforeDatabase(t *testing.T) {
	// Ordering guarantee: TLS validation (step 3) runs before database-file
	// accessibility (step 4). With both an invalid TLS config and an
	// inaccessible DB present, the TLS failure must win.
	if os.Geteuid() == 0 {
		t.Skip("running as root: read-permission checks are bypassed by the OS")
	}
	dir := writableDataDir(t)
	central := filepath.Join(dir, CentralDirectoryFileName)
	if err := os.WriteFile(central, []byte("not-a-db"), 0o000); err != nil {
		t.Fatalf("write central: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(central, 0o600) })

	cfg := baseConfig(dir)
	cfg.TLS = true // no cert/key => TLS failure

	_, err := Prepare(context.Background(), cfg, Options{})
	se, ok := AsStartupError(err)
	if !ok || se.Class != FailureTLS {
		t.Fatalf("expected FailureTLS to win over database error, got %v", err)
	}
}

// mustMigrate opens a SQLite database at path, runs the given migrate function,
// and closes it, failing the test on any error.
func mustMigrate(t *testing.T, path string, migrate func(context.Context, *sql.DB) error) {
	t.Helper()
	db, err := sql.Open(data.DriverName, path)
	if err != nil {
		t.Fatalf("open %q: %v", path, err)
	}
	defer db.Close()
	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate %q: %v", path, err)
	}
}
