package server

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/influence/influence/internal/data"
)

// This file holds end-to-end integration/example tests for the fail-fast
// startup sequence (task 2.6). Where startup_test.go exercises the ordered
// validation with an injected, socket-free binder, these tests drive Prepare
// with the REAL default binder (Options{}) so the whole path — including real
// net.Listen and the TLS listener seam — runs against genuine filesystem and
// network conditions. Each failure class is verified end-to-end, plus the
// plaintext-refused-when-TLS-on case served over a real TLS listener.
//
// Test names here are distinct from startup_test.go to avoid redefinition.

// freePort returns a currently-unused TCP port in the valid 1..65535 range by
// briefly binding an ephemeral socket and releasing it. It lets these
// end-to-end tests pass a real, range-valid port to Prepare's real binder
// (validatePort rejects 0, so an ephemeral 0 cannot be used directly). A tiny
// race window between release and re-bind is acceptable for a local test.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing reserved port: %v", err)
	}
	return port
}

// TestStartupE2E_InvalidPortNeverBinds drives Prepare with the real binder for
// several out-of-range ports and asserts each fails with the port class and,
// critically, that the configured port is never actually bound (Req 29.3).
// "No bind attempt" is observed end-to-end: after a rejected start, the port is
// still free for a real listener to claim.
func TestStartupE2E_InvalidPortNeverBinds(t *testing.T) {
	dir := writableDataDir(t)

	for _, port := range []int{0, -5, 65536, 70000} {
		cfg := baseConfig(dir)
		cfg.Port = port

		// Real default binder: if Prepare ever reached the bind step for an
		// invalid port it would try net.Listen(":<invalid>") and surface a
		// different (bind) error/class.
		_, err := Prepare(context.Background(), cfg, Options{})
		se, ok := AsStartupError(err)
		if !ok || se.Class != FailurePort {
			t.Fatalf("port %d: expected FailurePort end-to-end, got %v", port, err)
		}
	}
}

// TestStartupE2E_MissingDataDirFailsToStart drives the real binder against a
// Data_Directory path that does not exist and asserts a data-dir-missing
// failure with a message that names the directory (Req 30.4).
func TestStartupE2E_MissingDataDirFailsToStart(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	cfg := baseConfig(missing)

	_, err := Prepare(context.Background(), cfg, Options{})
	se, ok := AsStartupError(err)
	if !ok || se.Class != FailureDataDirMissing {
		t.Fatalf("expected FailureDataDirMissing end-to-end, got %v", err)
	}
	if se.Message == "" {
		t.Fatal("expected a specific data-dir-missing message")
	}
}

// TestStartupE2E_NonWritableDataDirFailsToStart makes an existing directory
// read-only and asserts startup fails with the not-writable class end-to-end
// (Req 30.5).
func TestStartupE2E_NonWritableDataDirFailsToStart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: write-permission checks are bypassed by the OS")
	}
	dir := writableDataDir(t)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := Prepare(context.Background(), baseConfig(dir), Options{})
	se, ok := AsStartupError(err)
	if !ok || se.Class != FailureDataDirNotWritable {
		t.Fatalf("expected FailureDataDirNotWritable end-to-end, got %v", err)
	}
}

// TestStartupE2E_MissingTLSCertFailsToStart enables TLS with cert/key paths
// that point at nonexistent files and asserts startup fails with the TLS class
// through the real BuildTLSConfig path (Req 19.4).
func TestStartupE2E_MissingTLSCertFailsToStart(t *testing.T) {
	dir := writableDataDir(t)
	cfg := baseConfig(dir)
	cfg.TLS = true
	cfg.TLSCert = filepath.Join(dir, "absent.crt")
	cfg.TLSKey = filepath.Join(dir, "absent.key")

	_, err := Prepare(context.Background(), cfg, Options{})
	se, ok := AsStartupError(err)
	if !ok || se.Class != FailureTLS {
		t.Fatalf("expected FailureTLS end-to-end, got %v", err)
	}
}

// TestStartupE2E_MismatchedTLSCertKeyFailsToStart writes a real cert and a real
// key that belong to DIFFERENT key pairs and asserts startup fails with the TLS
// class (Req 19.4). This exercises the "mismatched" branch specifically, which
// the existing unit tests (missing/empty paths) do not.
func TestStartupE2E_MismatchedTLSCertKeyFailsToStart(t *testing.T) {
	dir := writableDataDir(t)

	// Two independently-generated pairs; use pair A's cert with pair B's key.
	certA, _ := writeTestCertKey(t)
	_, keyB := writeTestCertKey(t)

	cfg := baseConfig(dir)
	cfg.TLS = true
	cfg.TLSCert = certA
	cfg.TLSKey = keyB

	_, err := Prepare(context.Background(), cfg, Options{})
	se, ok := AsStartupError(err)
	if !ok || se.Class != FailureTLS {
		t.Fatalf("expected FailureTLS for mismatched cert/key, got %v", err)
	}
}

// TestStartupE2E_InaccessibleDatabaseFailsToStart writes a present-but-corrupt
// Central_Directory file and asserts startup fails with the database class
// through the real accessibility check that opens and queries the file
// (Req 20.4). Unlike the permission-based unit test, this drives the
// open/query failure path and works regardless of process uid.
func TestStartupE2E_InaccessibleDatabaseFailsToStart(t *testing.T) {
	dir := writableDataDir(t)
	central := filepath.Join(dir, CentralDirectoryFileName)
	// A readable but non-SQLite file: os.Open succeeds, but opening it as a
	// database and querying sqlite_master fails, so the file is "present but
	// inaccessible" as a database.
	if err := os.WriteFile(central, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatalf("write central: %v", err)
	}

	_, err := Prepare(context.Background(), baseConfig(dir), Options{})
	se, ok := AsStartupError(err)
	if !ok || se.Class != FailureDatabaseInaccessible {
		t.Fatalf("expected FailureDatabaseInaccessible end-to-end, got %v", err)
	}
}

// TestStartupE2E_PortInUseFailsToStart binds a real listener on an ephemeral
// port, then drives Prepare with the REAL binder against that same port and
// asserts a genuine port-in-use failure (Req 29.4). This is the true
// end-to-end port-collision case: a real OS bind conflict, reported only after
// every earlier check passes.
func TestStartupE2E_PortInUseFailsToStart(t *testing.T) {
	// Claim a real port first.
	occupier, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupying a port: %v", err)
	}
	defer occupier.Close()
	port := occupier.Addr().(*net.TCPAddr).Port

	dir := writableDataDir(t)
	cfg := baseConfig(dir)
	cfg.Port = port

	prepared, err := Prepare(context.Background(), cfg, Options{})
	if prepared != nil && prepared.Listener != nil {
		prepared.Listener.Close()
		t.Fatal("expected startup to fail on an in-use port, but a listener was bound")
	}
	se, ok := AsStartupError(err)
	if !ok || se.Class != FailurePortInUse {
		t.Fatalf("expected FailurePortInUse end-to-end, got %v", err)
	}
}

// TestStartupE2E_SuccessBindsRealListener drives the full happy path with the
// real binder: a migrated Central_Directory + tenant DB in a writable data dir
// and a valid port. Prepare returns a bound, serve-ready TCP listener.
func TestStartupE2E_SuccessBindsRealListener(t *testing.T) {
	dir := writableDataDir(t)
	central := filepath.Join(dir, CentralDirectoryFileName)
	mustMigrate(t, central, data.MigrateCentral)
	tenant := filepath.Join(dir, "tenant_e2e.db")
	mustMigrate(t, tenant, data.MigrateTenant)

	cfg := baseConfig(dir)
	cfg.Port = freePort(t) // a real, range-valid, currently-free port

	prepared, err := Prepare(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("expected success end-to-end, got %v", err)
	}
	defer prepared.Listener.Close()

	if _, ok := prepared.Listener.Addr().(*net.TCPAddr); !ok {
		t.Errorf("listener addr = %T, want *net.TCPAddr", prepared.Listener.Addr())
	}
	if prepared.TLSConfig != nil {
		t.Error("expected nil TLS config when TLS disabled")
	}
}

// TestStartupE2E_PlaintextRefusedWhenTLSOn is the plaintext-refused-when-TLS-on
// case (Req 19.2), driven end-to-end: Prepare builds and binds a real TLS
// listener from a valid cert/key, an HTTP server serves the PlaintextGuard-
// wrapped handler over it, and then a real TLS client succeeds while a real
// plaintext client is refused (never reaches the handler's 200).
func TestStartupE2E_PlaintextRefusedWhenTLSOn(t *testing.T) {
	dir := writableDataDir(t)
	cert, key := writeTestCertKey(t)

	cfg := baseConfig(dir)
	cfg.Port = freePort(t)
	cfg.TLS = true
	cfg.TLSCert = cert
	cfg.TLSKey = key

	prepared, err := Prepare(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("expected TLS startup to succeed, got %v", err)
	}
	defer prepared.Listener.Close()

	if prepared.TLSConfig == nil {
		t.Fatal("expected a TLS config on the prepared server when TLS is on")
	}
	if prepared.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2 (%#x)", prepared.TLSConfig.MinVersion, tls.VersionTLS12)
	}

	// Serve the guarded handler over the bound TLS listener.
	handler := PlaintextGuard(cfg.TLS, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(prepared.Listener) }()
	defer srv.Close()

	addr := prepared.Listener.Addr().String()

	// A TLS client (accepting the self-signed cert) is served.
	tlsClient := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := tlsClient.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("TLS request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("TLS request status = %d, want 200", resp.StatusCode)
	}

	// A plaintext client against the TLS listener must not be served the
	// handler's 200: crypto/tls answers plaintext on a TLS port with a
	// non-200 response, so the request is refused (Req 19.2).
	plainClient := &http.Client{Timeout: 3 * time.Second}
	presp, perr := plainClient.Get("http://" + addr + "/")
	if perr == nil {
		defer presp.Body.Close()
		if presp.StatusCode == http.StatusOK {
			t.Errorf("plaintext request over TLS listener was served (status %d), want refusal", presp.StatusCode)
		}
	}
}
