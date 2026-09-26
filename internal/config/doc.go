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

// Package config resolves runtime configuration for the Influence server.
//
// Layered resolution applies the fixed precedence CLI flag > env var > config
// file > built-in default to every setting (see Resolve). The fail-fast startup
// validation of the resolved values lives in a later task; this package only
// resolves and returns the effective Config.
package config
