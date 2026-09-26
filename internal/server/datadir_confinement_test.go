package server

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/influence/influence/internal/config"
	"github.com/influence/influence/internal/data"
)

// Feature: influence, Property 30: Data directory confinement
//
// For all configured Data_Directories (defaulting to /var/lib/influence when
// unset), the resolved Central_Directory path and every Tenant_Database file
// path lie within the configured Data_Directory, and startup fails fast —
// before serving — when that directory does not exist or is not writable by
// the process.
//
// Validates: Requirements 30.2, 30.3, 30.4, 30.5
//
// This property drives the resolution/validation seam directly: the resolved
// Data_Directory comes from config.Resolve (Req 30.2 default), the confined
// paths are the ones startup validates and hands back (central =
// filepath.Join(dir, CentralDirectoryFileName); tenants are *.db files living
// inside dir — Req 30.3), and validateDataDir enforces the missing/non-writable
// fail-fast checks before any bind is attempted (Req 30.4, 30.5).
func TestDataDirectoryConfinementProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		root := t.TempDir()

		// Generate a Data_Directory *name* under a fresh temp root. Segment
		// characters are constrained to a portable, path-safe set so the
		// generator explores the input space intelligently without producing
		// paths that are impossible on the filesystem (which would test the OS,
		// not the confinement property).
		segChars := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"
		segGen := rapid.StringOfN(rapid.SampledFrom([]rune(segChars)), 1, 12, -1).
			Filter(func(s string) bool { return s != "." && s != ".." })
		segments := rapid.SliceOfN(segGen, 1, 3).Draw(rt, "dirSegments")
		dir := filepath.Join(append([]string{root}, segments...)...)

		// --- Req 30.2: default resolution when no Data_Directory is provided. ---
		// With no CLI flag, no env var, and no config file, Resolve must yield
		// the built-in default Data_Directory.
		emptyEnv := func(string) (string, bool) { return "", false }
		resolved, err := config.Resolve([]string{"--config", filepath.Join(root, "no-such-config")}, emptyEnv)
		if err != nil {
			rt.Fatalf("resolve with defaults: %v", err)
		}
		if resolved.DataDir != config.DefaultDataDir {
			rt.Fatalf("default Data_Directory = %q, want %q", resolved.DataDir, config.DefaultDataDir)
		}

		// An explicitly configured Data_Directory (via env) must win and be
		// carried through verbatim.
		envDir := func(k string) (string, bool) {
			if k == config.EnvDataDir {
				return dir, true
			}
			return "", false
		}
		resolvedDir, err := config.Resolve([]string{"--config", filepath.Join(root, "no-such-config")}, envDir)
		if err != nil {
			rt.Fatalf("resolve with configured dir: %v", err)
		}
		if resolvedDir.DataDir != dir {
			rt.Fatalf("configured Data_Directory = %q, want %q", resolvedDir.DataDir, dir)
		}

		// --- Req 30.4: a Data_Directory that does not exist fails fast. ---
		missingCalled := false
		_, missErr := Prepare(context.Background(), config.Config{Port: 8080, DataDir: dir},
			Options{BindListener: func(config.Config, *tls.Config) (net.Listener, error) {
				missingCalled = true
				return nil, nil
			}})
		se, ok := AsStartupError(missErr)
		if !ok || se.Class != FailureDataDirMissing {
			rt.Fatalf("missing dir %q: expected FailureDataDirMissing, got %v", dir, missErr)
		}
		if missingCalled {
			rt.Fatalf("missing dir %q: a bind was attempted before serving; startup must fail fast", dir)
		}

		// Now create the directory so confinement + writability can be checked.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			rt.Fatalf("mkdir %q: %v", dir, err)
		}

		// --- Req 30.3: resolved Central_Directory and Tenant_Database paths are
		// confined to the Data_Directory. ---
		prepared, prepErr := Prepare(context.Background(), config.Config{Port: 8080, DataDir: dir},
			Options{BindListener: ephemeralBinder})
		if prepErr != nil {
			rt.Fatalf("writable dir %q: expected success, got %v", dir, prepErr)
		}
		defer prepared.Listener.Close()

		if !isWithin(dir, prepared.CentralPath) {
			rt.Fatalf("central path %q escapes data dir %q", prepared.CentralPath, dir)
		}
		wantCentral := filepath.Join(dir, CentralDirectoryFileName)
		if prepared.CentralPath != wantCentral {
			rt.Fatalf("central path = %q, want %q", prepared.CentralPath, wantCentral)
		}

		// Materialize a handful of generated Tenant_Database files inside the
		// Data_Directory, then confirm startup discovers them and every
		// resolved tenant path stays within the directory (Req 30.3).
		tenantBases := rapid.SliceOfNDistinct(
			segGen, 0, 4,
			func(s string) string { return s },
		).Draw(rt, "tenantDBs")
		for _, base := range tenantBases {
			name := base + ".db"
			if name == CentralDirectoryFileName {
				continue
			}
			p := filepath.Join(dir, name)
			mustMigrate(t, p, data.MigrateTenant)
		}

		prepared2, prepErr2 := Prepare(context.Background(), config.Config{Port: 8080, DataDir: dir},
			Options{BindListener: ephemeralBinder})
		if prepErr2 != nil {
			rt.Fatalf("writable dir %q with tenants: expected success, got %v", dir, prepErr2)
		}
		defer prepared2.Listener.Close()

		for _, tp := range prepared2.TenantPaths {
			if !isWithin(dir, tp) {
				rt.Fatalf("tenant path %q escapes data dir %q", tp, dir)
			}
		}

		// --- Req 30.5: an existing but non-writable Data_Directory fails fast. ---
		// Skip when running as root, where the OS bypasses the permission bits
		// this check relies on.
		if os.Geteuid() != 0 {
			if err := os.Chmod(dir, 0o500); err != nil {
				rt.Fatalf("chmod %q: %v", dir, err)
			}
			nonWritableCalled := false
			_, nwErr := Prepare(context.Background(), config.Config{Port: 8080, DataDir: dir},
				Options{BindListener: func(config.Config, *tls.Config) (net.Listener, error) {
					nonWritableCalled = true
					return nil, nil
				}})
			// Restore perms so t.TempDir cleanup can remove the tree.
			_ = os.Chmod(dir, 0o700)

			se, ok := AsStartupError(nwErr)
			if !ok || se.Class != FailureDataDirNotWritable {
				rt.Fatalf("non-writable dir %q: expected FailureDataDirNotWritable, got %v", dir, nwErr)
			}
			if nonWritableCalled {
				rt.Fatalf("non-writable dir %q: a bind was attempted; startup must fail fast", dir)
			}
		}
	})
}

// ephemeralBinder hands back a no-op listener so a successful Prepare has
// something to return. Property 30 asserts path confinement and fail-fast
// behaviour, not real socket binding (that is Property 31), so this avoids
// opening a real OS socket on each of the property's many iterations.
func ephemeralBinder(config.Config, *tls.Config) (net.Listener, error) {
	return &fakeListener{}, nil
}

// fakeListener is a net.Listener that never accepts a connection. Prepare only
// needs a closeable handle to return; it never serves in this property.
type fakeListener struct{}

func (*fakeListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (*fakeListener) Close() error              { return nil }
func (*fakeListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0} }

// isWithin reports whether path resolves to a location inside dir (or is dir
// itself). It compares cleaned paths so ".." traversal cannot smuggle a path
// outside the Data_Directory past the check.
func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
}
