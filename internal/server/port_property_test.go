package server

import (
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/influence/influence/internal/config"
	"pgregory.net/rapid"
)

// Feature: influence, Property 31: Listen port selection and validation
//
// For all port inputs, the server binds and serves on the given port when it is
// an integer in 1..65535 and available, uses 8080 when no port is provided, and
// fails fast without serving when the value is out of range or non-integer
// (making no bind attempt) or when the requested port is already in use.
//
// Validates: Requirements 29.1, 29.2, 29.3, 29.4
//
// The property exercises the two collaborating pieces of port handling:
//   - config.Resolve, which parses/defaults the --port flag (Req 29.1, 29.2 and
//     the non-integer half of 29.3), and
//   - server.Prepare / validatePort, which range-checks the resolved port and
//     binds last (the out-of-range half of 29.3 and the in-use case of 29.4).
//
// A recording binder proves Req 29.3's "make no attempt to bind a port": for
// every invalid port the binder must never be invoked. Availability vs
// in-use (29.4) is modelled with injected binders so the test never depends on
// which real OS ports happen to be free.

// recordingBinder captures whether Prepare ever attempted to bind, and on which
// port, so the property can assert the no-bind-on-invalid-port guarantee and
// that a successful bind targets the resolved port.
type recordingBinder struct {
	called   bool
	seenPort int
	simulate func(port int) (net.Listener, error)
}

func (b *recordingBinder) bind(cfg config.Config, tlsCfg *tls.Config) (net.Listener, error) {
	b.called = true
	b.seenPort = cfg.Port
	if b.simulate != nil {
		return b.simulate(cfg.Port)
	}
	// Default: bind an ephemeral socket so a "successful" bind returns a real,
	// closeable listener without colliding on the configured port.
	return net.Listen("tcp", "127.0.0.1:0")
}

func TestProperty31_ListenPortSelectionAndValidation(t *testing.T) {
	// A single writable Data_Directory suffices: the property varies the port,
	// not the data dir, and Prepare only probes-and-removes a temp file there.
	dir := t.TempDir()

	rapid.Check(t, func(t *rapid.T) {
		// Choose one of several input shapes so the single property covers the
		// full decision space of Req 29.1–29.4.
		kind := rapid.SampledFrom([]string{
			"valid_available",
			"valid_in_use",
			"absent_default",
			"out_of_range",
			"non_integer",
		}).Draw(t, "kind")

		switch kind {
		case "valid_available":
			// Req 29.1: a valid, available port binds and serves on that port.
			port := rapid.IntRange(PortMin, PortMax).Draw(t, "port")
			cfg, err := config.Resolve([]string{"--port", strconv.Itoa(port), "--data-dir", dir}, envNone)
			if err != nil {
				t.Fatalf("Resolve rejected a valid port %d: %v", port, err)
			}
			if cfg.Port != port {
				t.Fatalf("resolved port = %d, want %d", cfg.Port, port)
			}
			binder := &recordingBinder{}
			prepared, err := Prepare(context.Background(), cfg, Options{BindListener: binder.bind})
			if err != nil {
				t.Fatalf("port %d: expected success, got %v", port, err)
			}
			if !binder.called {
				t.Fatalf("port %d: a valid available port must reach the bind step", port)
			}
			if binder.seenPort != port {
				t.Fatalf("bind targeted port %d, want the resolved port %d", binder.seenPort, port)
			}
			if prepared.Listener == nil {
				t.Fatalf("port %d: expected a bound listener", port)
			}
			_ = prepared.Listener.Close()

		case "valid_in_use":
			// Req 29.4: a valid port that is already in use fails fast on bind.
			port := rapid.IntRange(PortMin, PortMax).Draw(t, "port")
			cfg, err := config.Resolve([]string{"--port", strconv.Itoa(port), "--data-dir", dir}, envNone)
			if err != nil {
				t.Fatalf("Resolve rejected a valid port %d: %v", port, err)
			}
			binder := &recordingBinder{
				simulate: func(int) (net.Listener, error) {
					return nil, &net.OpError{Op: "listen", Err: net.UnknownNetworkError("address already in use")}
				},
			}
			_, err = Prepare(context.Background(), cfg, Options{BindListener: binder.bind})
			se, ok := AsStartupError(err)
			if !ok || se.Class != FailurePortInUse {
				t.Fatalf("port %d in use: expected FailurePortInUse, got %v", port, err)
			}
			if !binder.called {
				t.Fatalf("port %d: in-use is only detectable after a bind attempt", port)
			}

		case "absent_default":
			// Req 29.2: with no --port flag the resolved port is the default.
			cfg, err := config.Resolve([]string{"--data-dir", dir}, envNone)
			if err != nil {
				t.Fatalf("Resolve failed with no port flag: %v", err)
			}
			if cfg.Port != config.DefaultPort {
				t.Fatalf("absent port resolved to %d, want default %d", cfg.Port, config.DefaultPort)
			}
			binder := &recordingBinder{}
			prepared, err := Prepare(context.Background(), cfg, Options{BindListener: binder.bind})
			if err != nil {
				t.Fatalf("default port: expected success, got %v", err)
			}
			if binder.seenPort != config.DefaultPort {
				t.Fatalf("bind targeted port %d, want default %d", binder.seenPort, config.DefaultPort)
			}
			_ = prepared.Listener.Close()

		case "out_of_range":
			// Req 29.3 (range half): an integer outside 1..65535 fails fast and
			// makes no bind attempt. Resolve accepts the integer; validatePort
			// rejects it before the bind step.
			port := rapid.OneOf(
				rapid.IntRange(-1<<20, PortMin-1),
				rapid.IntRange(PortMax+1, PortMax+1<<20),
			).Draw(t, "port")
			cfg, err := config.Resolve([]string{"--port", strconv.Itoa(port), "--data-dir", dir}, envNone)
			if err != nil {
				t.Fatalf("Resolve should accept the integer %d (range check is Prepare's job): %v", port, err)
			}
			binder := &recordingBinder{}
			_, err = Prepare(context.Background(), cfg, Options{BindListener: binder.bind})
			se, ok := AsStartupError(err)
			if !ok || se.Class != FailurePort {
				t.Fatalf("port %d: expected FailurePort, got %v", port, err)
			}
			if binder.called {
				t.Fatalf("port %d: no bind must be attempted for an out-of-range port", port)
			}

		case "non_integer":
			// Req 29.3 (non-integer half): a non-integer --port value is
			// rejected at resolution, so startup never reaches a bind.
			raw := rapid.StringMatching(`[A-Za-z][A-Za-z0-9]*`).Draw(t, "raw")
			// Guard against the generator producing something strconv.Atoi
			// would parse (it won't for this pattern, but keep the property
			// honest about its precondition).
			if _, convErr := strconv.Atoi(strings.TrimSpace(raw)); convErr == nil {
				return
			}
			_, err := config.Resolve([]string{"--port", raw, "--data-dir", dir}, envNone)
			if err == nil {
				t.Fatalf("non-integer port %q was accepted; expected a resolution error", raw)
			}
		}
	})
}

// envNone is a lookupEnv that reports every variable as unset, keeping the
// property hermetic from the real process environment.
func envNone(string) (string, bool) { return "", false }
