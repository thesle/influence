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

// Package ws stands up the WebSocket hub that backs concurrent editing
// (design.md — "Concurrency Service — Req 25"; Requirement 25.1).
//
// Every Polygon operates in exactly one of two mutually exclusive edit modes,
// recorded in POLYGON.edit_mode (Req 25.1): collaborative editing or record
// locking. The hub organizes connected editors into per-Polygon rooms — one
// room is the set of clients editing a single Polygon, keyed by the caller's
// tenant plus the Polygon's Record_ID so a room can never span two tenants
// (Req 1.5). Within a room the hub supports join, leave, and broadcast, which
// are the primitives both concurrency paths build on: collaborative CRDT
// signaling (Req 25.2, task 18.4) and record-lock EDIT_LOCK signaling
// (Req 25.3–25.5, task 18.2).
//
// This package is deliberately split so the room bookkeeping is transport- and
// storage-agnostic and therefore unit-testable:
//
//   - Hub / room / Client (hub.go) own membership and fan-out over an abstract
//     Client. They know nothing about WebSockets or SQLite.
//   - ModeResolver (mode.go) tells the hub which single mode a Polygon is in,
//     so a room follows exactly one concurrency path.
//   - The connection handler (conn.go) upgrades an HTTP request to a WebSocket,
//     wraps it as a Client, and joins/leaves the room — the only part that
//     depends on the WebSocket library.
//
// The lock-acquire/reject/release logic and the CRDT merge itself live in later
// tasks (18.2 and 18.4); this package only provides the hub, rooms, and mode
// resolution they plug into.
package ws
