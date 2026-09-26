package main_test

// Integration tests for task 25.3: config / port / data-dir startup validation
// driven through the ACTUAL compiled binary (not the in-process Prepare seam
// exercised by internal/server/startup_test.go). These build cmd/influence
// once, then launch it as a child process under each failure condition and
// assert on the process EXIT CODE and the operator-facing ERROR MESSAGE printed
// to stderr.
//
// These tests run WITHOUT root or systemd and must always execute (not skip)
// in a normal CI/dev environment.
//
//   - invalid / out-of-range port fails fast with NO bind attempt   (Req 29.3)
//   - a port already in use fails to start                          (Req 29.4)
//   - a missing Data_Directory fails to start                       (Req 30.4)
//   - a non-writable Data_Directory fails to start                  (Req 30.5)
//
// The binary logs startup failures via log.Fatalf, which writes the message to
// stderr and exits with status 1. main() prints two message prefixes:
//   "influence: configuration error: ..."  (config could not be resolved)
//   "influence: startup failed: ..."        (a fail-fast validation rejected it)
// with the StartupError's class-specific message appended (see
// internal/server/startup.go).

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildOnce compiles the influence binary a single time for the whole test
// binary and hands back its path. Building once keeps these end-to-end tests
// fast while still exercising the real, linked program.
var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

func influenceBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "influence-bin-*")
		if err != nil {
			buildErr = err
			return
		}
		bin := filepath.Join(dir, "influence")
		// Build the current package (cmd/influence). CGO stays off to match the
		// production static build; the pure-Go SQLite driver needs no cgo.
		cmd := exec.Command("go", "build", "-o", bin, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = errors.New("go build: " + err.Error() + "\n" + string(out))
			return
		}
		builtBin = bin
	})
	if buildErr != nil {
		t.Fatalf("building influence binary: %v", buildErr)
	}
	return builtBin
}

// runInfluence launches the built binary with args, giving it a moment to run
// and then terminating it if it is still alive (a successful start would block
// serving). It returns the combined stdout+stderr and the process exit state:
// exited reports whether the process exited on its own (a fail-fast rejection)
// versus having to be killed (it started serving).
type runResult struct {
	output   string
	exited   bool // true if the process terminated on its own before the deadline
	exitCode int  // valid only when exited is true
}

func runInfluence(t *testing.T, bin string, args ...string) runResult {
	t.Helper()

	cmd := exec.Command(bin, args...)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", bin, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		// The process exited on its own — the fail-fast path.
		res := runResult{output: buf.String(), exited: true}
		if err == nil {
			res.exitCode = 0
			return res
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.exitCode = ee.ExitCode()
			return res
		}
		t.Fatalf("waiting for %s: %v", bin, err)
	case <-time.After(3 * time.Second):
		// Still running after the grace period: it bound the listener and began
		// serving. Kill it and report "did not exit".
		_ = cmd.Process.Kill()
		<-done
		return runResult{output: buf.String(), exited: false}
	}
	return runResult{}
}

// TestBinary_InvalidPortFailsFastNoBind launches the real binary with several
// out-of-range ports and asserts each exits non-zero with the invalid-port
// message — never a bind/"in use" error, which proves no bind was attempted
// (Req 29.3). A writable data dir isolates the failure to the port check.
func TestBinary_InvalidPortFailsFastNoBind(t *testing.T) {
	bin := influenceBinary(t)
	dataDir := t.TempDir()

	for _, port := range []string{"0", "-5", "65536", "70000"} {
		t.Run("port="+port, func(t *testing.T) {
			res := runInfluence(t, bin,
				"--config", os.DevNull,
				"--port", port,
				"--data-dir", dataDir,
			)
			if !res.exited {
				t.Fatalf("port %s: process kept running; an invalid port must fail fast", port)
			}
			if res.exitCode == 0 {
				t.Fatalf("port %s: exit code 0; expected non-zero on invalid port", port)
			}
			// The rejection must be the port-validation message, not a bind
			// error — that is the observable "no bind attempt" (Req 29.3).
			if !strings.Contains(res.output, "listen port") ||
				!strings.Contains(res.output, "is invalid") {
				t.Fatalf("port %s: expected an invalid-port message, got:\n%s", port, res.output)
			}
			if strings.Contains(res.output, "already in use") {
				t.Fatalf("port %s: got a bind/in-use error; a bind must not be attempted for an invalid port:\n%s", port, res.output)
			}
		})
	}
}

// TestBinary_PortInUseFailsToStart occupies a real ephemeral port, then starts
// the binary configured to bind that same port and asserts it exits non-zero
// with the port-in-use message (Req 29.4). This is the genuine OS bind
// conflict, reported only after every earlier check passes.
func TestBinary_PortInUseFailsToStart(t *testing.T) {
	bin := influenceBinary(t)
	dataDir := t.TempDir()

	occupier, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupying a port: %v", err)
	}
	defer occupier.Close()
	port := occupier.Addr().(*net.TCPAddr).Port

	res := runInfluence(t, bin,
		"--config", os.DevNull,
		"--port", strconv.Itoa(port),
		"--data-dir", dataDir,
	)
	if !res.exited {
		t.Fatalf("process kept running; a port-in-use bind must fail fast")
	}
	if res.exitCode == 0 {
		t.Fatalf("exit code 0; expected non-zero when the port is in use")
	}
	if !strings.Contains(res.output, "already in use") {
		t.Fatalf("expected a port-in-use message, got:\n%s", res.output)
	}
}

// TestBinary_MissingDataDirFailsToStart points the binary at a Data_Directory
// that does not exist and asserts it exits non-zero naming the directory
// (Req 30.4).
func TestBinary_MissingDataDirFailsToStart(t *testing.T) {
	bin := influenceBinary(t)
	missing := filepath.Join(t.TempDir(), "no-such-dir")

	res := runInfluence(t, bin,
		"--config", os.DevNull,
		"--port", "8080",
		"--data-dir", missing,
	)
	if !res.exited {
		t.Fatalf("process kept running; a missing data dir must fail fast")
	}
	if res.exitCode == 0 {
		t.Fatalf("exit code 0; expected non-zero for a missing data dir")
	}
	if !strings.Contains(res.output, "data directory") ||
		!strings.Contains(res.output, "does not exist") {
		t.Fatalf("expected a missing-data-dir message, got:\n%s", res.output)
	}
}

// TestBinary_NonWritableDataDirFailsToStart makes an existing directory
// read-only and asserts the binary refuses to start (Req 30.5). Skipped when
// running as root, where the OS bypasses write-permission bits.
func TestBinary_NonWritableDataDirFailsToStart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: write-permission checks are bypassed by the OS")
	}
	bin := influenceBinary(t)
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dataDir, 0o700) })

	res := runInfluence(t, bin,
		"--config", os.DevNull,
		"--port", "8080",
		"--data-dir", dataDir,
	)
	if !res.exited {
		t.Fatalf("process kept running; a non-writable data dir must fail fast")
	}
	if res.exitCode == 0 {
		t.Fatalf("exit code 0; expected non-zero for a non-writable data dir")
	}
	if !strings.Contains(res.output, "data directory") ||
		!strings.Contains(res.output, "not writable") {
		t.Fatalf("expected a non-writable-data-dir message, got:\n%s", res.output)
	}
}
