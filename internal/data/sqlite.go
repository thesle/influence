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

package data

// The Influence platform persists everything in SQLite with no separate
// database server (Requirement 20.3). Search requires SQLite FTS5
// (Requirement 23), so the driver must be built with FTS5 support.
//
// We use modernc.org/sqlite, a pure-Go SQLite driver that ships FTS5 support
// without cgo, keeping the build to a single static binary (Requirements 20.1,
// 20.2). Importing it here registers the "sqlite" database/sql driver for the
// rest of the backend and anchors the dependency in go.mod.
import (
	_ "modernc.org/sqlite"
)

// DriverName is the database/sql driver name registered by the imported SQLite
// driver. Later tasks open Central_Directory and Tenant_Database handles with
// this name.
const DriverName = "sqlite"
