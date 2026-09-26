package deploy_test

// Integration test for task 25.3: install → uninstall → re-run idempotence and
// Data_Directory preservation, exercised against the real Makefile targets in a
// systemd-capable environment (Requirements 28.4, 28.5, 28.6, 28.7).
//
// `make install` and `make uninstall` create a system user, write under
// /usr/local/bin, /etc and /var/lib, and drive systemctl — all of which require
// root and a working systemd. That is NOT available in an ordinary CI/dev
// sandbox, so this test SKIPS cleanly unless every precondition is met:
//
//   - the INFLUENCE_INSTALL_IT=1 env gate is set (explicit opt-in, so the test
//     never mutates a host that merely happens to be root),
//   - the process is root (euid 0),
//   - systemctl is on PATH and the system is booted with systemd.
//
// When it does run it asserts:
//   - `make install` then `make install` again is idempotent (Req 28.4),
//   - a marker file written into the Data_Directory survives `make uninstall`
//     (data preserved by default — Req 28.6),
//   - `make uninstall` is idempotent when re-run (Req 28.5, 28.7),
//   - `make uninstall PURGE_DATA=1` removes the Data_Directory (Req 28.6).
//
// The test restores the host to its pre-test state (uninstall + purge) via
// t.Cleanup so it leaves no service, binary, or data behind.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// installITEnv is the opt-in gate. Without it the test skips even as root, so
// the destructive install path never runs implicitly.
const installITEnv = "INFLUENCE_INSTALL_IT"

// These paths mirror the Makefile install targets. Keep them in sync with the
// Makefile if those locations change.
const (
	installedBin = "/usr/local/bin/influence"
	dataDir      = "/var/lib/influence"
	unitDst      = "/etc/systemd/system/influence.service"
)

// requireInstallEnv skips unless the opt-in gate is set and the environment can
// actually perform a systemd install (root + systemctl + booted systemd).
func requireInstallEnv(t *testing.T) {
	t.Helper()
	if os.Getenv(installITEnv) != "1" {
		t.Skipf("skipping install/uninstall IT: set %s=1 to enable (requires root + systemd)", installITEnv)
	}
	if os.Geteuid() != 0 {
		t.Skip("skipping install/uninstall IT: must run as root")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("skipping install/uninstall IT: systemctl not found on PATH")
	}
	// `systemctl is-system-running` returns non-zero for "degraded"/"offline"
	// but still speaks to a live manager; a hard failure (e.g. no D-Bus, not
	// booted with systemd) means we cannot drive units.
	if out, err := exec.Command("systemctl", "is-system-running").CombinedOutput(); err != nil {
		state := strings.TrimSpace(string(out))
		// "degraded" is acceptable (some unrelated unit failed); "offline" /
		// exec errors are not.
		if state != "degraded" && state != "running" && state != "starting" && state != "maintenance" {
			t.Skipf("skipping install/uninstall IT: systemd not usable (state=%q, err=%v)", state, err)
		}
	}
}

// repoRoot returns the module root (parent of this deploy/ directory) so the
// test can invoke `make -C <root>` regardless of the working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// This test lives in <root>/deploy, so the module root is one level up.
	root := filepath.Dir(wd)
	if _, err := os.Stat(filepath.Join(root, "Makefile")); err != nil {
		t.Fatalf("Makefile not found at repo root %q: %v", root, err)
	}
	return root
}

// runMake invokes a Makefile target from the repo root and fails the test on a
// non-zero exit, surfacing the combined output for diagnosis.
func runMake(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("make", append([]string{"-C", root}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestInstallUninstallIdempotenceAndDataPreservation(t *testing.T) {
	requireInstallEnv(t)
	root := repoRoot(t)

	// Always leave the host clean, even if an assertion fails midway.
	t.Cleanup(func() {
		_ = exec.Command("make", "-C", root, "uninstall", "PURGE_DATA=1").Run()
	})

	// 1. Install, then install again — the second run must succeed (idempotent,
	//    Req 28.4) and end in the same state (binary + unit + data dir present).
	runMake(t, root, "install")
	runMake(t, root, "install")

	for _, p := range []string{installedBin, dataDir, unitDst} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("after install, expected %q to exist: %v", p, err)
		}
	}

	// 2. Write a marker into the Data_Directory to prove uninstall preserves
	//    operator data (Req 28.6).
	marker := filepath.Join(dataDir, "it-marker.txt")
	if err := os.WriteFile(marker, []byte("preserve-me"), 0o640); err != nil {
		t.Fatalf("writing data-dir marker: %v", err)
	}

	// 3. Uninstall (default): service + binary + unit removed, data preserved.
	runMake(t, root, "uninstall")
	if _, err := os.Stat(installedBin); !os.IsNotExist(err) {
		t.Fatalf("after uninstall, binary %q should be gone (err=%v)", installedBin, err)
	}
	if _, err := os.Stat(unitDst); !os.IsNotExist(err) {
		t.Fatalf("after uninstall, unit %q should be gone (err=%v)", unitDst, err)
	}
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("after uninstall, data dir %q must be preserved: %v", dataDir, err)
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "preserve-me" {
		t.Fatalf("after uninstall, marker must be preserved (content=%q, err=%v)", string(b), err)
	}

	// 4. Uninstall again — idempotent even with nothing installed (Req 28.5,
	//    28.7); data still preserved.
	runMake(t, root, "uninstall")
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("after second uninstall, data dir %q must still be preserved: %v", dataDir, err)
	}

	// 5. Re-install, then uninstall with PURGE_DATA=1 — the data dir is removed
	//    (Req 28.6).
	runMake(t, root, "install")
	runMake(t, root, "uninstall", "PURGE_DATA=1")
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("after uninstall PURGE_DATA=1, data dir %q should be removed (err=%v)", dataDir, err)
	}
}
