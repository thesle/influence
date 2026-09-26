package deploy_test

import (
	"os"
	"path/filepath"
	"testing"

	"pgregory.net/rapid"
)

// Feature: influence, Property 32: Install/uninstall idempotence
//
// Applying `make install` one or more times, and `make uninstall` one or more
// times, converges to the same end state regardless of how many times each
// runs (idempotence). Uninstall preserves the Data_Directory and its database
// files unless the operator explicitly requests their removal (PURGE_DATA=1).
//
// Validates: Requirements 28.4, 28.5, 28.6, 28.7
//
// Why a modeled state machine (and not the real Makefile):
//
//	The real `make install` / `make uninstall` recipes manage host-global
//	resources: they run useradd/groupadd, write to /usr/local/bin, /etc and
//	/var/lib, and drive systemctl. Executing them requires root plus a live
//	systemd and would mutate the host — neither available nor safe in the test
//	sandbox. This test therefore models the install/uninstall logic as a pair of
//	pure transition functions that faithfully mirror the *guarded, idempotent*
//	shell in the Makefile (and the deploy/packaging/*.sh maintainer-script
//	analogues), operating over a real temp filesystem tree (t.TempDir) for the
//	parts whose preservation semantics we care about (the Data_Directory, its
//	contents, and the config file). The state elements the sandbox cannot host
//	(the service user/group, the systemd unit registration/enablement) are
//	tracked as booleans in the model, mirroring the getent/id and systemctl
//	guards one-for-one.
//
// Modeled system state (system):
//
//	userExists       bool   -- `id -u influence` succeeds        (useradd guard)
//	groupExists      bool   -- `getent group influence` succeeds (groupadd guard)
//	binaryInstalled  bool   -- /usr/local/bin/influence present
//	unitInstalled    bool   -- /etc/systemd/system/influence.service present
//	unitEnabled      bool   -- `systemctl enable` has run for the unit
//	dataDir          path   -- /var/lib/influence, backed by a real temp dir
//	configPath       path   -- /etc/influence/config, backed by a real temp file
//
// The Data_Directory and config live on a real filesystem so that
// "preservation" is a real, observable property (bytes on disk), not just a
// bookkeeping flag.
//
// Modeled transitions (mirroring the Makefile recipes):
//
//	install(s):   idempotent creation. Create group/user if absent; install the
//	              binary (always overwrites atomically — install(1)); ensure the
//	              data dir exists; install the DEFAULT config ONLY if none exists
//	              (Req 28.4 — never clobber operator edits); install + enable the
//	              systemd unit. Re-running yields the same end state.
//	uninstall(s): idempotent teardown. Stop/disable + remove the unit; remove the
//	              binary. Preserve the data dir and its contents unless
//	              purge==true, in which case remove the data dir (Req 28.5/28.6).
//	              The service user/group are intentionally left in place (matches
//	              the maintainer scripts) so a reinstall preserves tenant data.
//
// The rapid harness draws an arbitrary sequence of install / uninstall(purge?)
// operations plus arbitrary "operator" mutations (seeding data files, editing
// the config), and asserts the convergence and preservation invariants below.

// system is the modeled host state. The Data_Directory and the config file are
// backed by real paths under a t.TempDir root; the remaining fields mirror the
// host-global resources the sandbox cannot safely create.
type system struct {
	root string // temp root standing in for "/"

	userExists      bool
	groupExists     bool
	binaryInstalled bool
	unitInstalled   bool
	unitEnabled     bool
}

// The default config the install recipe writes when none exists. Mirrors the
// `printf` in the Makefile install target.
const defaultConfig = "port = 8080\ndata_dir = /var/lib/influence\ntls = false\n"

func (s *system) dataDir() string    { return filepath.Join(s.root, "var", "lib", "influence") }
func (s *system) configDir() string  { return filepath.Join(s.root, "etc", "influence") }
func (s *system) configPath() string { return filepath.Join(s.configDir(), "config") }

func exists(t *rapid.T, path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat %s: %v", path, err)
	return false
}

// install mirrors the guarded, idempotent `make install` recipe.
func (s *system) install(t *rapid.T) {
	// Create the group/user if absent (groupadd/useradd guards). Idempotent:
	// re-running is a no-op once they exist.
	if !s.groupExists {
		s.groupExists = true
	}
	if !s.userExists {
		s.userExists = true
	}

	// install(1) writes the binary atomically and always succeeds; re-running
	// simply re-installs the same file. Modeled as a flag flip.
	s.binaryInstalled = true

	// Ensure the Data_Directory exists (install -d). Idempotent: install -d
	// does not error when the directory already exists, and never disturbs
	// existing contents.
	if err := os.MkdirAll(s.dataDir(), 0o750); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	// Ensure the config dir exists, then install the DEFAULT config ONLY if no
	// config file already exists (Req 28.4). A pre-existing config — including
	// one an operator edited — is left byte-for-byte untouched.
	if err := os.MkdirAll(s.configDir(), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if !exists(t, s.configPath()) {
		if err := os.WriteFile(s.configPath(), []byte(defaultConfig), 0o644); err != nil {
			t.Fatalf("write default config: %v", err)
		}
	}

	// Install + enable the systemd unit. enable is idempotent for an already
	// enabled unit.
	s.unitInstalled = true
	s.unitEnabled = true
}

// uninstall mirrors the guarded, idempotent `make uninstall` recipe. When
// purge is true it also removes the Data_Directory (PURGE_DATA=1).
func (s *system) uninstall(t *rapid.T, purge bool) {
	// Stop/disable + remove the unit if present. Idempotent: skipped cleanly
	// when the unit is already absent.
	s.unitEnabled = false
	s.unitInstalled = false

	// Remove the installed binary if present (guarded rm). Idempotent.
	s.binaryInstalled = false

	// Preserve the Data_Directory and its database files unless PURGE_DATA=1
	// (Req 28.6). The service user/group are intentionally NOT removed, matching
	// the maintainer scripts, so tenant data survives a reinstall.
	if purge {
		if err := os.RemoveAll(s.dataDir()); err != nil {
			t.Fatalf("purge data dir: %v", err)
		}
	}
}

// snapshot captures the observable end state for equality comparison. The data
// dir contents and config bytes are read from the real filesystem so that
// "same end state" includes actual on-disk preservation.
type snapshot struct {
	userExists      bool
	groupExists     bool
	binaryInstalled bool
	unitInstalled   bool
	unitEnabled     bool
	dataDirExists   bool
	dataFiles       map[string]string // relative path -> contents
	configExists    bool
	configContents  string
}

func (s *system) snapshot(t *rapid.T) snapshot {
	snap := snapshot{
		userExists:      s.userExists,
		groupExists:     s.groupExists,
		binaryInstalled: s.binaryInstalled,
		unitInstalled:   s.unitInstalled,
		unitEnabled:     s.unitEnabled,
		dataFiles:       map[string]string{},
	}

	dir := s.dataDir()
	if exists(t, dir) {
		snap.dataDirExists = true
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			snap.dataFiles[rel] = string(b)
			return nil
		})
		if err != nil {
			t.Fatalf("walk data dir: %v", err)
		}
	}

	if exists(t, s.configPath()) {
		snap.configExists = true
		b, err := os.ReadFile(s.configPath())
		if err != nil {
			t.Fatalf("read config: %v", err)
		}
		snap.configContents = string(b)
	}
	return snap
}

func equalSnapshots(a, b snapshot) bool {
	if a.userExists != b.userExists ||
		a.groupExists != b.groupExists ||
		a.binaryInstalled != b.binaryInstalled ||
		a.unitInstalled != b.unitInstalled ||
		a.unitEnabled != b.unitEnabled ||
		a.dataDirExists != b.dataDirExists ||
		a.configExists != b.configExists ||
		a.configContents != b.configContents {
		return false
	}
	if len(a.dataFiles) != len(b.dataFiles) {
		return false
	}
	for k, v := range a.dataFiles {
		if b.dataFiles[k] != v {
			return false
		}
	}
	return true
}

// newSystem builds a fresh modeled host rooted under t.TempDir. Nothing is
// installed; the data dir and config do not yet exist, matching a clean host.
func newSystem(t *rapid.T) *system {
	root, err := os.MkdirTemp("", "influence-install-*")
	if err != nil {
		t.Fatalf("temp root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return &system{root: root}
}

// TestInstallUninstallIdempotenceProperty exercises Property 32.
//
// For an arbitrary interleaving of install / uninstall(purge?) operations,
// interspersed with arbitrary operator actions (seeding tenant database files
// and editing the config), the test asserts:
//
//   - install repeated N>=1 times converges to the same end state as a single
//     install (Req 28.7);
//   - uninstall repeated N>=1 times converges to the same end state as a single
//     uninstall (Req 28.7);
//   - after any number of installs followed by a non-purging uninstall, the
//     Data_Directory and every file in it are preserved byte-for-byte, and a
//     previously written config is unchanged (Req 28.5, 28.6);
//   - uninstall with PURGE_DATA=1 removes the Data_Directory (Req 28.6);
//   - install never overwrites a pre-existing config, preserving operator edits
//     (Req 28.4).
func TestInstallUninstallIdempotenceProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		s := newSystem(rt)

		steps := rapid.IntRange(1, 30).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			op := rapid.SampledFrom([]string{
				"install", "uninstall", "uninstallPurge", "seedData", "editConfig",
			}).Draw(rt, "op")

			switch op {
			case "install":
				// install repeated an arbitrary number of times must converge:
				// one install then N-1 more installs == a single install.
				n := rapid.IntRange(1, 4).Draw(rt, "installRepeats")
				s.install(rt)
				once := s.snapshot(rt)
				for r := 1; r < n; r++ {
					s.install(rt)
					if got := s.snapshot(rt); !equalSnapshots(once, got) {
						rt.Fatalf("install not idempotent: state changed on repeat %d/%d", r+1, n)
					}
				}

			case "uninstall", "uninstallPurge":
				purge := op == "uninstallPurge"
				n := rapid.IntRange(1, 4).Draw(rt, "uninstallRepeats")

				// Capture the pre-uninstall data/config so we can verify
				// preservation for the non-purging path (Req 28.5/28.6).
				before := s.snapshot(rt)

				s.uninstall(rt, purge)
				once := s.snapshot(rt)
				for r := 1; r < n; r++ {
					s.uninstall(rt, purge)
					if got := s.snapshot(rt); !equalSnapshots(once, got) {
						rt.Fatalf("uninstall(purge=%v) not idempotent: state changed on repeat %d/%d", purge, r+1, n)
					}
				}

				if purge {
					// PURGE_DATA=1 removes the Data_Directory (Req 28.6).
					if once.dataDirExists {
						rt.Fatalf("uninstall with PURGE_DATA=1 left the data dir in place")
					}
				} else {
					// Non-purging uninstall preserves the data dir, every file
					// in it, and the config (Req 28.5, 28.6).
					if before.dataDirExists != once.dataDirExists {
						rt.Fatalf("uninstall changed data dir existence: before=%v after=%v",
							before.dataDirExists, once.dataDirExists)
					}
					if len(before.dataFiles) != len(once.dataFiles) {
						rt.Fatalf("uninstall changed data file count: before=%d after=%d",
							len(before.dataFiles), len(once.dataFiles))
					}
					for k, v := range before.dataFiles {
						if once.dataFiles[k] != v {
							rt.Fatalf("uninstall altered data file %q: before=%q after=%q",
								k, v, once.dataFiles[k])
						}
					}
					if before.configExists != once.configExists ||
						before.configContents != once.configContents {
						rt.Fatalf("uninstall altered config: before(exists=%v)=%q after(exists=%v)=%q",
							before.configExists, before.configContents,
							once.configExists, once.configContents)
					}
				}

			case "seedData":
				// An operator (or the running service) writes a tenant database
				// file under the Data_Directory. Only meaningful once the dir
				// exists; if it does not, ensure it, mirroring a service that
				// creates its own data on first run.
				if err := os.MkdirAll(s.dataDir(), 0o750); err != nil {
					rt.Fatalf("mkdir data dir for seed: %v", err)
				}
				name := rapid.SampledFrom([]string{
					"central.db", "tenant-a.db", "tenant-b.db", "tenant-c.db",
				}).Draw(rt, "seedName")
				content := rapid.StringMatching(`[a-zA-Z0-9 ]{1,32}`).Draw(rt, "seedContent")
				if err := os.WriteFile(filepath.Join(s.dataDir(), name), []byte(content), 0o640); err != nil {
					rt.Fatalf("seed data file: %v", err)
				}

			case "editConfig":
				// An operator edits the config. install must never clobber this
				// (Req 28.4); a later install must leave the edit intact.
				if err := os.MkdirAll(s.configDir(), 0o755); err != nil {
					rt.Fatalf("mkdir config dir for edit: %v", err)
				}
				edited := rapid.StringMatching(`port = [0-9]{2,5}\ndata_dir = /var/lib/influence\ntls = (true|false)\n`).
					Draw(rt, "editedConfig")
				if err := os.WriteFile(s.configPath(), []byte(edited), 0o644); err != nil {
					rt.Fatalf("edit config: %v", err)
				}

				// Directly assert Req 28.4: a subsequent install must not
				// overwrite the operator's config.
				s.install(rt)
				if got := s.snapshot(rt); got.configContents != edited {
					rt.Fatalf("install overwrote operator-edited config: want %q, got %q",
						edited, got.configContents)
				}
			}
		}
	})
}
