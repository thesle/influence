package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"pgregory.net/rapid"
)

// envMap builds a lookupEnv function from a map for hermetic tests (no reliance
// on the real process environment).
func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestResolveDefaults(t *testing.T) {
	// No flags, no env, no config file present at the default path we point at.
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "does-not-exist")

	cfg, err := Resolve([]string{"--config", missing}, envMap(nil))
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("Port = %d, want default %d", cfg.Port, DefaultPort)
	}
	if cfg.DataDir != DefaultDataDir {
		t.Errorf("DataDir = %q, want default %q", cfg.DataDir, DefaultDataDir)
	}
	if cfg.TLS != DefaultTLS {
		t.Errorf("TLS = %v, want default %v", cfg.TLS, DefaultTLS)
	}
	if cfg.TLSCert != "" || cfg.TLSKey != "" {
		t.Errorf("TLS cert/key = %q/%q, want empty", cfg.TLSCert, cfg.TLSKey)
	}
	if cfg.MasterSecret != "" {
		t.Errorf("MasterSecret = %q, want empty", cfg.MasterSecret)
	}
}

func TestResolveConfigFileLayer(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config")
	body := `# influence config
port = 9090
data_dir = /srv/influence
tls = true
tls_cert = /etc/influence/cert.pem
tls_key   /etc/influence/key.pem
master_secret = "s3cr3t"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Resolve([]string{"--config", path}, envMap(nil))
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.DataDir != "/srv/influence" {
		t.Errorf("DataDir = %q, want /srv/influence", cfg.DataDir)
	}
	if !cfg.TLS {
		t.Errorf("TLS = false, want true")
	}
	if cfg.TLSCert != "/etc/influence/cert.pem" {
		t.Errorf("TLSCert = %q", cfg.TLSCert)
	}
	if cfg.TLSKey != "/etc/influence/key.pem" {
		t.Errorf("TLSKey = %q", cfg.TLSKey)
	}
	if cfg.MasterSecret != "s3cr3t" {
		t.Errorf("MasterSecret = %q, want s3cr3t", cfg.MasterSecret)
	}
}

// TestResolvePrecedence exercises CLI > env > file > default for the port and
// data-dir settings.
func TestResolvePrecedence(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config")
	if err := os.WriteFile(path, []byte("port = 3000\ndata_dir = /from/file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("file over default", func(t *testing.T) {
		cfg, err := Resolve([]string{"--config", path}, envMap(nil))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Port != 3000 || cfg.DataDir != "/from/file" {
			t.Errorf("got port=%d dataDir=%q, want 3000 /from/file", cfg.Port, cfg.DataDir)
		}
	})

	t.Run("env over file", func(t *testing.T) {
		env := envMap(map[string]string{EnvPort: "4000", EnvDataDir: "/from/env"})
		cfg, err := Resolve([]string{"--config", path}, env)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Port != 4000 || cfg.DataDir != "/from/env" {
			t.Errorf("got port=%d dataDir=%q, want 4000 /from/env", cfg.Port, cfg.DataDir)
		}
	})

	t.Run("cli over env and file", func(t *testing.T) {
		env := envMap(map[string]string{EnvPort: "4000", EnvDataDir: "/from/env"})
		cfg, err := Resolve([]string{"--config", path, "--port", "5000", "--data-dir", "/from/cli"}, env)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Port != 5000 || cfg.DataDir != "/from/cli" {
			t.Errorf("got port=%d dataDir=%q, want 5000 /from/cli", cfg.Port, cfg.DataDir)
		}
	})
}

func TestResolveConfigPathPrecedence(t *testing.T) {
	tmp := t.TempDir()
	flagPathFile := filepath.Join(tmp, "flag-config")
	envPathFile := filepath.Join(tmp, "env-config")
	if err := os.WriteFile(flagPathFile, []byte("port = 1111\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPathFile, []byte("port = 2222\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// --config wins over INFLUENCE_CONFIG for the path itself.
	cfg, err := Resolve([]string{"--config", flagPathFile}, envMap(map[string]string{EnvConfig: envPathFile}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigPath != flagPathFile {
		t.Errorf("ConfigPath = %q, want %q", cfg.ConfigPath, flagPathFile)
	}
	if cfg.Port != 1111 {
		t.Errorf("Port = %d, want 1111 (from --config file)", cfg.Port)
	}

	// With only the env var, the env path is used.
	cfg, err = Resolve(nil, envMap(map[string]string{EnvConfig: envPathFile}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigPath != envPathFile || cfg.Port != 2222 {
		t.Errorf("got ConfigPath=%q Port=%d, want %q 2222", cfg.ConfigPath, cfg.Port, envPathFile)
	}
}

func TestResolveTLSFlagForms(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "none")

	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"bare --tls", []string{"--tls"}, true},
		{"--tls=true", []string{"--tls=true"}, true},
		{"--tls=false", []string{"--tls=false"}, false},
		{"absent", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--config", missing}, tc.args...)
			cfg, err := Resolve(args, envMap(nil))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.TLS != tc.want {
				t.Errorf("TLS = %v, want %v", cfg.TLS, tc.want)
			}
		})
	}
}

func TestResolveRejectsNonIntegerPort(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "none")
	_, err := Resolve([]string{"--config", missing, "--port", "notanumber"}, envMap(nil))
	if err == nil {
		t.Fatal("expected error for non-integer port, got nil")
	}
}

func TestResolveRejectsUnknownFlag(t *testing.T) {
	_, err := Resolve([]string{"--nope", "x"}, envMap(nil))
	if err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
}

func TestResolveMalformedConfigFile(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config")
	if err := os.WriteFile(path, []byte("this-line-has-no-separator\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve([]string{"--config", path}, envMap(nil)); err == nil {
		t.Fatal("expected error for malformed config line, got nil")
	}
}

// TestResolvePortPrecedenceProperty verifies that whichever highest-precedence
// layer supplies the port is the one that wins, across many generated values
// and layer combinations. This complements the example-based precedence test
// with broad input coverage.
func TestResolvePortPrecedenceProperty(t *testing.T) {
	tmp := t.TempDir()
	rapid.Check(t, func(t *rapid.T) {
		portGen := rapid.IntRange(1, 65535)

		cliSet := rapid.Bool().Draw(t, "cliSet")
		envSet := rapid.Bool().Draw(t, "envSet")
		fileSet := rapid.Bool().Draw(t, "fileSet")

		cliVal := portGen.Draw(t, "cliVal")
		envVal := portGen.Draw(t, "envVal")
		fileVal := portGen.Draw(t, "fileVal")

		dir, err := os.MkdirTemp(tmp, "case")
		if err != nil {
			t.Fatalf("mkdir temp: %v", err)
		}
		cfgPath := filepath.Join(dir, "config")
		if fileSet {
			if err := os.WriteFile(cfgPath, []byte("port = "+strconv.Itoa(fileVal)+"\n"), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
		}

		args := []string{"--config", cfgPath}
		if cliSet {
			args = append(args, "--port", strconv.Itoa(cliVal))
		}
		env := map[string]string{}
		if envSet {
			env[EnvPort] = strconv.Itoa(envVal)
		}

		cfg, err := Resolve(args, envMap(env))
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}

		var want int
		switch {
		case cliSet:
			want = cliVal
		case envSet:
			want = envVal
		case fileSet:
			want = fileVal
		default:
			want = DefaultPort
		}
		if cfg.Port != want {
			t.Fatalf("Port = %d, want %d (cli=%v env=%v file=%v)", cfg.Port, want, cliSet, envSet, fileSet)
		}
	})
}
