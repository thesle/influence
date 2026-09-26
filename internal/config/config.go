// Influence — a self-hostable documentation platform.
// Copyright (C) 2026  Conrad Smith
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Default values for every configurable setting (the lowest-precedence layer).
//
// These match the design's configuration table (§ Command-line interface and
// configuration): CLI flag > env var > config file > built-in default.
const (
	// DefaultPort is the listen port used when nothing else supplies one
	// (Requirement 29.2).
	DefaultPort = 8080

	// DefaultDataDir is the Data_Directory used when nothing else supplies one
	// (Requirement 30.2). It holds the Central_Directory and every
	// Tenant_Database file.
	DefaultDataDir = "/var/lib/influence"

	// DefaultConfigPath is the config-file path used when neither --config nor
	// INFLUENCE_CONFIG is provided.
	DefaultConfigPath = "/etc/influence/config"

	// DefaultTLS is the default TLS-enabled state (disabled).
	DefaultTLS = false
)

// Environment-variable names for each setting. CLI flags and config-file keys
// are defined alongside the flag set and parser below.
const (
	EnvPort         = "INFLUENCE_PORT"
	EnvDataDir      = "INFLUENCE_DATA_DIR"
	EnvConfig       = "INFLUENCE_CONFIG"
	EnvTLS          = "INFLUENCE_TLS"
	EnvTLSCert      = "INFLUENCE_TLS_CERT"
	EnvTLSKey       = "INFLUENCE_TLS_KEY"
	EnvMasterSecret = "INFLUENCE_MASTER_SECRET"
)

// Config holds the fully-resolved runtime configuration. Every field is
// resolved exactly once at startup from the layered sources and then treated as
// immutable by the rest of the server.
//
// This task (2.1) is responsible only for layered resolution. Validation of the
// effective values and the fail-fast startup sequence live in task 2.2, so
// Config carries the resolved values without asserting they are valid.
type Config struct {
	// Port is the resolved listen port (Requirement 29.1). Task 2.2 validates
	// it is an integer in 1–65535.
	Port int

	// DataDir is the resolved Data_Directory holding the Central_Directory and
	// all Tenant_Database files (Requirement 30.1).
	DataDir string

	// ConfigPath is the path the config file was read from (or the resolved
	// default when no file was present).
	ConfigPath string

	// TLS reports whether in-process TLS termination is enabled (Requirement
	// 19.1).
	TLS bool

	// TLSCert and TLSKey are the PEM certificate and private-key paths;
	// required when TLS is enabled (Requirement 19.3).
	TLSCert string
	TLSKey  string

	// MasterSecret is the server master secret used to derive per-tenant
	// key-encryption keys (design § Key derivation and cipher).
	MasterSecret string
}

// fileConfig captures which keys were actually present in the config file so
// that a present-but-empty value still overrides a lower-precedence layer while
// an absent key does not.
type fileConfig struct {
	values map[string]string
}

func (f fileConfig) lookup(key string) (string, bool) {
	if f.values == nil {
		return "", false
	}
	v, ok := f.values[key]
	return v, ok
}

// Resolve builds a Config by applying the fixed precedence CLI flag > env var >
// config file > built-in default to every setting (Requirements 29.1, 29.2,
// 30.1, 30.2, 19.3, and the server master secret).
//
// args are the process arguments excluding the program name (typically
// os.Args[1:]). lookupEnv reads environment variables (typically os.LookupEnv);
// injecting it keeps resolution testable without mutating the real environment.
//
// Resolve performs no validation of the effective values — that is task 2.2. It
// only fails when the provided flags or the referenced config file cannot be
// parsed at all.
func Resolve(args []string, lookupEnv func(string) (string, bool)) (Config, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}

	flags, err := parseFlags(args)
	if err != nil {
		return Config{}, err
	}

	// Precedence for the config-file path itself: --config > INFLUENCE_CONFIG >
	// default. The config file cannot specify its own path, so there is no
	// file layer for this setting.
	configPath := resolveString(flags.get(flagConfig), envValue(lookupEnv, EnvConfig), nil, "", DefaultConfigPath)

	file, err := loadConfigFile(configPath)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		ConfigPath: configPath,
	}

	// Port: CLI > env > file > default.
	portStr := resolveString(flags.get(flagPort), envValue(lookupEnv, EnvPort), fileValue(file, keyPort), "", "")
	if portStr == "" {
		cfg.Port = DefaultPort
	} else {
		port, perr := strconv.Atoi(strings.TrimSpace(portStr))
		if perr != nil {
			// Preserve the offending value so task 2.2 can report it as
			// invalid (Requirement 29.3). A non-integer resolves to a sentinel
			// that fails range validation without attempting a bind.
			return Config{}, fmt.Errorf("config: port %q is not an integer", portStr)
		}
		cfg.Port = port
	}

	cfg.DataDir = resolveString(flags.get(flagDataDir), envValue(lookupEnv, EnvDataDir), fileValue(file, keyDataDir), "", DefaultDataDir)

	cfg.TLS = resolveBool(flags.tlsSet, flags.tls, envValue(lookupEnv, EnvTLS), fileValue(file, keyTLS), DefaultTLS)

	cfg.TLSCert = resolveString(flags.get(flagTLSCert), envValue(lookupEnv, EnvTLSCert), fileValue(file, keyTLSCert), "", "")
	cfg.TLSKey = resolveString(flags.get(flagTLSKey), envValue(lookupEnv, EnvTLSKey), fileValue(file, keyTLSKey), "", "")

	cfg.MasterSecret = resolveString(nil, envValue(lookupEnv, EnvMasterSecret), fileValue(file, keyMasterSecret), "", "")

	return cfg, nil
}

// resolveString applies the precedence order to a string setting. Each argument
// is a pointer to the value at that layer, or nil when the layer did not supply
// the setting. A present-but-empty value at a higher layer still wins over
// lower layers, matching "resolved once at startup" semantics.
func resolveString(cli, env, file *string, _ string, def string) string {
	if cli != nil {
		return *cli
	}
	if env != nil {
		return *env
	}
	if file != nil {
		return *file
	}
	return def
}

// resolveBool applies the precedence order to a boolean setting. cliSet reports
// whether the flag was present on the command line.
func resolveBool(cliSet bool, cliVal bool, env, file *string, def bool) bool {
	if cliSet {
		return cliVal
	}
	if env != nil {
		return parseBool(*env, def)
	}
	if file != nil {
		return parseBool(*file, def)
	}
	return def
}

func parseBool(s string, def bool) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return v
}

// envValue returns a pointer to the environment value for key, or nil when the
// variable is unset. An explicitly-empty variable counts as present.
func envValue(lookupEnv func(string) (string, bool), key string) *string {
	if v, ok := lookupEnv(key); ok {
		return &v
	}
	return nil
}

// fileValue returns a pointer to the config-file value for key, or nil when the
// key is absent from the file.
func fileValue(file fileConfig, key string) *string {
	if v, ok := file.lookup(key); ok {
		return &v
	}
	return nil
}

// loadConfigFile reads a simple line-based `key = value` config file. Blank
// lines and lines beginning with '#' are ignored; inline values may use either
// `key = value` or `key value`. A missing file is not an error here: absence
// simply means the file layer supplies nothing and lower-precedence defaults
// apply. Task 2.2 owns any requirement that the file must exist.
func loadConfigFile(path string) (fileConfig, error) {
	if path == "" {
		return fileConfig{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileConfig{}, nil
		}
		return fileConfig{}, fmt.Errorf("config: reading %q: %w", path, err)
	}
	defer f.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := splitConfigLine(line)
		if !ok {
			return fileConfig{}, fmt.Errorf("config: %q line %d: malformed entry %q", path, lineNo, line)
		}
		values[key] = val
	}
	if err := scanner.Err(); err != nil {
		return fileConfig{}, fmt.Errorf("config: reading %q: %w", path, err)
	}
	return fileConfig{values: values}, nil
}

// splitConfigLine splits a config line into a key and value. It accepts
// `key = value` and `key value`, trimming surrounding whitespace and optional
// surrounding quotes on the value.
func splitConfigLine(line string) (key, value string, ok bool) {
	var rest string
	if i := strings.IndexByte(line, '='); i >= 0 {
		key = strings.TrimSpace(line[:i])
		rest = strings.TrimSpace(line[i+1:])
	} else if i := strings.IndexAny(line, " \t"); i >= 0 {
		key = strings.TrimSpace(line[:i])
		rest = strings.TrimSpace(line[i+1:])
	} else {
		return "", "", false
	}
	if key == "" {
		return "", "", false
	}
	value = unquote(rest)
	return key, value, true
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
