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
	"fmt"
	"strconv"
	"strings"
)

// CLI flag names (without the leading "--"). These mirror the design's
// configuration table.
const (
	flagPort    = "port"
	flagDataDir = "data-dir"
	flagConfig  = "config"
	flagTLS     = "tls"
	flagTLSCert = "tls-cert"
	flagTLSKey  = "tls-key"
)

// Config-file keys. These mirror the design's configuration table. The config
// file cannot set its own path, so there is no key for --config. The master
// secret is resolvable from the file as well.
const (
	keyPort         = "port"
	keyDataDir      = "data_dir"
	keyTLS          = "tls"
	keyTLSCert      = "tls_cert"
	keyTLSKey       = "tls_key"
	keyMasterSecret = "master_secret"
)

// parsedFlags holds the string flags that were explicitly provided on the
// command line, plus the boolean --tls flag's presence and value. Only flags
// actually present participate in precedence resolution; absent flags fall
// through to the next layer.
type parsedFlags struct {
	strings map[string]string
	tlsSet  bool
	tls     bool
}

func (p parsedFlags) get(name string) *string {
	if p.strings == nil {
		return nil
	}
	if v, ok := p.strings[name]; ok {
		return &v
	}
	return nil
}

// stringFlags is the set of flags that take a value.
var stringFlags = map[string]bool{
	flagPort:    true,
	flagDataDir: true,
	flagConfig:  true,
	flagTLSCert: true,
	flagTLSKey:  true,
}

// parseFlags parses the Influence CLI flags from args (excluding the program
// name). It supports the GNU-style forms `--flag value`, `--flag=value`, and
// the boolean `--tls` / `--tls=true|false`. Single-dash forms (`-flag`) are
// also accepted for convenience. Unknown flags are rejected so a mistyped flag
// fails fast rather than being silently ignored.
func parseFlags(args []string) (parsedFlags, error) {
	pf := parsedFlags{strings: make(map[string]string)}

	i := 0
	for i < len(args) {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			return parsedFlags{}, fmt.Errorf("config: unexpected argument %q", arg)
		}
		name := strings.TrimLeft(arg, "-")
		if name == "" {
			return parsedFlags{}, fmt.Errorf("config: malformed flag %q", arg)
		}

		// Split --flag=value form.
		var inlineVal string
		hasInline := false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			inlineVal = name[eq+1:]
			name = name[:eq]
			hasInline = true
		}

		switch {
		case name == flagTLS:
			if hasInline {
				v, err := strconv.ParseBool(inlineVal)
				if err != nil {
					return parsedFlags{}, fmt.Errorf("config: --%s=%q is not a boolean", flagTLS, inlineVal)
				}
				pf.tls = v
			} else {
				pf.tls = true
			}
			pf.tlsSet = true
			i++
		case stringFlags[name]:
			if hasInline {
				pf.strings[name] = inlineVal
				i++
				continue
			}
			if i+1 >= len(args) {
				return parsedFlags{}, fmt.Errorf("config: flag --%s requires a value", name)
			}
			pf.strings[name] = args[i+1]
			i += 2
		default:
			return parsedFlags{}, fmt.Errorf("config: unknown flag --%s", name)
		}
	}

	return pf, nil
}
