# Implementation Plan: Influence

## Overview

This plan converts the Influence design into an incremental, test-driven Go + SvelteKit build. It starts from project scaffolding (Go module, FTS5-enabled SQLite build, SvelteKit app) and layers up through configuration/CLI with fail-fast startup, the two-tier data model and migrations, the tenant connection manager (structural isolation), authentication, authorization/PolicyEngine, then the content domain (Spheres, Facets, Polygons), the markdown pipeline, encryption + centralized masking, image blobs, metadata, navigation, PDF export, search, concurrent editing, bookmarks, admin, tenant create/export, documentation, and finally the Makefile/systemd/packaging.

Each task builds on prior tasks and wires new code into the running system so nothing is orphaned. Every task references specific requirement sub-clauses. Property tests (one per Correctness Property, minimum 100 iterations, tagged `// Feature: influence, Property {n}: {property text}`) and unit/integration tests are attached as sub-tasks and marked optional with `*`. Property 18 (masking never leaks plaintext/ciphertext) is the highest-priority security test.

Testing libraries: Go property tests use `pgregory.net/rapid`; frontend property tests use `fast-check`. Do NOT implement property-based testing from scratch — use these established libraries.

## Tasks

- [x] 1. Project scaffolding and build foundation
  - [x] 1.1 Initialize Go module and backend layout
    - Create `go.mod` (Go 1.22+), directory layout (`cmd/influence`, `internal/{config,server,middleware,data,service,repo}`, `deploy`, `docs`)
    - Add `chi` router and the FTS5-enabled SQLite driver dependency (`mattn/go-sqlite3` with the `sqlite_fts5` build tag, or `modernc.org/sqlite` configured for FTS5)
    - Add `pgregory.net/rapid` as a test dependency
    - Provide a minimal `main` that parses no config yet but compiles and runs
    - _Requirements: 20.1, 20.2, 20.3_
  - [x] 1.2 Add an FTS5-availability smoke test
    - Open an in-memory SQLite DB, create an FTS5 virtual table, assert it succeeds (guards the build tag/driver choice)
    - _Requirements: 20.3, 23.1_
  - [x] 1.3 Scaffold the SvelteKit frontend app
    - Create the SvelteKit project (`frontend/`) with a static/node adapter, base layout shell, and a dev proxy to the Go API
    - Add `fast-check` as a frontend dev dependency
    - _Requirements: 21.1_

- [x] 2. Configuration, CLI, and fail-fast startup
  - [x] 2.1 Implement layered configuration resolution
    - Resolve every setting with precedence CLI flag > env var > config file > default: `--port`/`INFLUENCE_PORT`/`port` (default 8080), `--data-dir`/`INFLUENCE_DATA_DIR`/`data_dir` (default `/var/lib/influence`), `--config`/`INFLUENCE_CONFIG` (default `/etc/influence/config`), `--tls`, `--tls-cert`, `--tls-key`, and the server master secret
    - _Requirements: 29.1, 29.2, 30.1, 30.2, 19.3_
  - [x] 2.2 Implement the fixed fail-fast startup sequence
    - In order: (1) validate port is integer in 1–65535, making no bind attempt on failure; (2) validate Data_Directory exists and is writable; (3) load/validate TLS cert+key when TLS enabled; (4) confirm Central_Directory + Tenant_Database files are accessible; (5) bind the listener, failing fast if the port is already in use. Only begin serving after all steps pass
    - Emit a specific error message per failure class
    - _Requirements: 29.3, 29.4, 30.3, 30.4, 30.5, 19.4, 20.4_
  - [x] 2.3 Implement TLS listener and plaintext enforcement
    - When TLS is enabled, bind only a TLS listener with `MinVersion = tls.VersionTLS12`; refuse/redirect plaintext requests
    - _Requirements: 19.1, 19.2_
  - [x] 2.4 Property test for listen-port selection and validation
    - **Property 31: Listen port selection and validation**
    - **Validates: Requirements 29.1, 29.2, 29.3, 29.4**
  - [x] 2.5 Property test for data-directory confinement
    - **Property 30: Data directory confinement**
    - **Validates: Requirements 30.2, 30.3, 30.4, 30.5**
  - [x] 2.6 Integration/example tests for startup fail-fast
    - Invalid/out-of-range port makes no bind attempt (Req 29.3); port-in-use fails (Req 29.4); missing/non-writable Data_Directory fails (Req 30.4, 30.5); missing/mismatched TLS cert/key fails to start (Req 19.4); inaccessible DB file fails to start (Req 20.4); plaintext refused when TLS on (Req 19.2)
    - _Requirements: 19.2, 19.4, 20.4, 29.3, 29.4, 30.4, 30.5_

- [x] 3. Two-tier data model and migrations
  - [x] 3.1 Create Central_Directory schema and migrations
    - Tables: `TENANT`, `USER` (Argon2id `password_hash`, `failed_attempts`, `locked_until`, `deactivated`), `GROUP` (`is_admin`), `GROUP_MEMBERSHIP`, `SESSION`. Enforce identity/registry only — no content columns
    - _Requirements: 1.1, 1.2, 3.4, 4.1, 5.4_
  - [x] 3.2 Create Tenant_Database schema and migrations
    - Tables: `SPHERE`, `FACET`, `POLYGON`, `CIPHERTEXT`, `IMAGE_BLOB`, `SPHERE_GRANT`, `TENANT_SECRET`, `EDIT_LOCK`, `USER_CIRCLE`, `BOOKMARK`, `VIEW_LOG`, and the `polygon_fts` FTS5 virtual table. Record `record_id` UNIQUE columns for Sphere/Facet/Polygon
    - _Requirements: 2.1, 2.3, 6.3, 8.3, 10.4, 15.4_
  - [x] 3.3 Implement Record_ID generation and encoding
    - Generate UUIDv4 stored canonically plus a base57 Short-UUID URL form; assign at creation, never mutate; unique per record type within the tenant
    - _Requirements: 15.4, 15.5, 15.2_
  - [x] 3.4 Property test for Record_ID uniqueness and immutability
    - **Property 9: Record_ID uniqueness and immutability**
    - **Validates: Requirements 15.4, 15.5**

- [x] 4. Tenant connection manager and structural isolation
  - [x] 4.1 Implement the ConnManager and RequestContext
    - `Central() *sql.DB`; `Tenant(ctx) (*TenantDB, error)` returning ONLY the handle for `ctx.TenantID`; a per-tenant pool map with LRU/idle eviction. No API to open an arbitrary tenant outside the caller's authenticated tenant
    - Define immutable `RequestContext{UserID, TenantID, Groups, SphereGrants}`
    - _Requirements: 1.4, 1.5_
  - [x] 4.2 Enforce tenant-scoped record lookups
    - All content repositories take `ctx` and resolve records only within `ctx.TenantID`'s DB; a Record_ID absent from that tenant returns "not accessible" with no cross-tenant query path and no data change
    - _Requirements: 1.5, 1.6_
  - [x] 4.3 Property test for tenant isolation
    - **Property 1: Tenant isolation**
    - **Validates: Requirements 1.2, 1.5, 1.6**

- [x] 5. Authentication service
  - [x] 5.1 Implement password hashing and login
    - Store/verify credentials with Argon2id in Central_Directory; on success resolve the user's single tenant and create a server-side session; deny with an auth error on invalid credentials
    - _Requirements: 3.1, 3.2, 3.4, 1.3, 1.4_
  - [x] 5.2 Implement brute-force lockout
    - Track consecutive failures; the 5th consecutive failure sets `locked_until = now + 15m`; reject attempts during lockout regardless of correctness; reset the counter on success
    - _Requirements: 3.3_
  - [x] 5.3 Implement session lifecycle and expiry
    - Opaque high-entropy token in an HttpOnly/Secure/SameSite cookie; session valid iff `now - last_seen_at <= 30m` AND `now - created_at <= 24h`; slide `last_seen_at` per request; logout deletes the session
    - _Requirements: 3.5, 3.6_
  - [x] 5.4 Unit/example tests for lockout and session boundaries
    - Assert lockout triggers exactly on the 5th consecutive failure; assert the 30-minute inactivity and 24-hour absolute expiry boundaries; assert logout invalidates the session
    - _Requirements: 3.3, 3.5, 3.6_

- [x] 6. Authorization, PolicyEngine, and middleware chain
  - [x] 6.1 Implement the PolicyEngine
    - `CanAccessSphere` (union of Group grants, most-permissive access), `CanReveal` (true iff any granting Group has Reveal), `IsAdmin`; Admin_Group yields implicit write+reveal on every Sphere in the tenant; deactivated users are denied all Spheres and admin pages while retaining account + memberships
    - _Requirements: 4.3, 4.5, 4.7, 5.4_
  - [x] 6.2 Implement the ordered middleware chain
    - TLS-enforce → Session (validate, slide, expire) → Tenant-resolve (attach TenantID) → Authz (load grants, check Sphere/admin access, attach per-Sphere CanReveal). Downstream services trust RequestContext and never re-derive tenant from client input
    - Return `403 admin privileges required` for non-admins on admin routes; `403` no-Sphere-access error otherwise
    - _Requirements: 4.5, 5.5, 3.5, 3.6, 1.4_
  - [x] 6.3 Property test for effective per-Sphere access
    - **Property 3: Effective per-Sphere access**
    - **Validates: Requirements 4.3, 4.5, 4.7**
  - [x] 6.4 Unit/example test for admin-route denial
    - Non-admin request to an admin route returns admin-required error; deactivated user is denied Spheres and admin pages
    - _Requirements: 5.5, 5.4_

- [x] 7. User and Group management
  - [x] 7.1 Implement user/group membership rules and grants
    - Enforce ≥1 Group membership at all times; grant a Group access to 0..N Spheres with access level + Reveal; apply/revoke grants to current members; revoke on grant loss unless retained via another Group
    - _Requirements: 4.1, 4.2, 4.3, 4.4, 4.6_
  - [x] 7.2 Property test for minimum group-membership invariant
    - **Property 2: Minimum group membership invariant**
    - **Validates: Requirements 4.1**

- [x] 8. Spheres: CRUD, cascade, circling, ordering
  - [x] 8.1 Implement Sphere CRUD with validation and cascade delete
    - Create with name 1–100 chars, unique within tenant; reject empty/oversize/duplicate with a validation error leaving the Sphere set unchanged; delete cascades to all Facets and Polygons in a single transaction
    - _Requirements: 6.1, 6.2, 6.4, 6.5, 6.3_
  - [x] 8.2 Implement circling and per-user ordering
    - Circle adds to the user's Circled_Sphere list (idempotent); un-circle removes and returns to alphabetical remainder; persist/apply per-user circled order; non-circled shown ascending case-insensitive by name with Record_ID tie-break; circled shown above remainder
    - _Requirements: 7.1, 7.2, 7.3, 7.4, 7.5, 7.6_
  - [x] 8.3 Property test for Sphere name validation and uniqueness
    - **Property 4: Sphere name validation and uniqueness**
    - **Validates: Requirements 6.4**
  - [x] 8.4 Property test for Sphere display ordering
    - **Property 5: Sphere display ordering**
    - **Validates: Requirements 7.3, 7.4, 7.5**
  - [x] 8.5 Property test for circling idempotence and un-circle round-trip
    - **Property 6: Circling idempotence and un-circle round-trip**
    - **Validates: Requirements 7.2, 7.6**

- [x] 9. Facets: hierarchy and integrity
  - [x] 9.1 Implement Facet CRUD with validation and cascade delete
    - Create with name 1–100 chars within a Sphere; reject empty/oversize; a Facet may contain child Facets; delete cascades to descendant Facets and contained Polygons in one transaction
    - _Requirements: 8.1, 8.2, 8.4, 8.5, 8.6, 8.3_
  - [x] 9.2 Implement reparent hierarchy integrity
    - Reject setting a Facet's parent to itself, a descendant, or a Facet in a different Sphere; maintain single-parent, acyclic forest; leave hierarchy unchanged on rejection
    - _Requirements: 9.1, 9.2, 9.3_
  - [x] 9.3 Property test for Facet hierarchy integrity
    - **Property 7: Facet hierarchy integrity**
    - **Validates: Requirements 9.1, 9.2, 9.3**

- [x] 10. Checkpoint - Ensure all tests pass
  - Ensure all tests pass, ask the user if questions arise.

- [x] 11. Polygons: types, storage, folder containment
  - [x] 11.1 Implement Polygon CRUD with type validity and persistence rollback
    - Support exactly Markdown_Page, Folder_Polygon, Tabular_Polygon, Whiteboard_Polygon; reject invalid types creating nothing; store within a Sphere; on persistence failure do not persist and leave the Sphere unchanged; Folder_Polygon may contain other Polygons including folders; unique Record_ID within tenant
    - _Requirements: 10.1, 10.2, 10.3, 10.4, 10.5, 10.6, 10.7_
  - [x] 11.2 Property test for Polygon type validity
    - **Property 8: Polygon type validity**
    - **Validates: Requirements 10.1, 10.3**

- [x] 12. Encryption service and centralized Masker
  - [x] 12.1 Implement tenant key material and derivation
    - At tenant creation generate a random 128-bit Tenant_Salt and a random 256-bit DEK; wrap the DEK with a KEK derived via Argon2id from the server master secret salted with Tenant_Salt; store Tenant_Salt + wrapped DEK in TENANT_SECRET inside the tenant DB
    - _Requirements: 16.1, 16.9, 2.2_
  - [x] 12.2 Implement span encryption and obfuscation
    - Encrypt a selected span with AES-256-GCM (random 96-bit nonce); store ciphertext keyed by a unique token id in CIPHERTEXT; replace the span in POLYGON.content with `|encrypt|{id}|`, retaining no plaintext copy
    - _Requirements: 16.1, 16.2_
  - [x] 12.3 Implement the centralized Masker
    - Single component that turns tokens into output given `reveal bool` from context; masks tokens by default; used by render, PDF export, and search-index population so plaintext and ciphertext never reach an unauthorized egress
    - _Requirements: 16.3, 16.7, 22.2, 23.2_
  - [x] 12.4 Implement per-token reveal with per-Sphere permission
    - Reveal a single token only after re-checking CanReveal for the token's owning Sphere; return that token's plaintext while all others stay masked; deny reveal (content stays masked, "not permitted") for users lacking permission; on missing/undecryptable ciphertext keep masked and return "reveal failed" with no partial plaintext
    - _Requirements: 16.4, 16.5, 16.6, 16.8_
  - [x] 12.5 Property test — masking never leaks plaintext or ciphertext (HIGHEST PRIORITY)
    - **Property 18: Masking never leaks plaintext or ciphertext**
    - **Validates: Requirements 16.3, 16.7, 22.2, 23.2**
    - Must assert absence of BOTH plaintext and ciphertext across render, PDF-text, and search-index outputs for a no-reveal context
  - [x] 12.6 Property test for encryption obfuscation removes plaintext
    - **Property 16: Encryption obfuscation removes plaintext**
    - **Validates: Requirements 16.1, 16.2**
  - [x] 12.7 Property test for encrypt→reveal round-trip
    - **Property 17: Encrypt→reveal round-trip**
    - **Validates: Requirements 16.1, 16.4**
  - [x] 12.8 Property test for per-token reveal isolation
    - **Property 19: Per-token reveal isolation**
    - **Validates: Requirements 16.4**
  - [x] 12.9 Property test for per-Sphere reveal permission
    - **Property 20: Reveal permission is per-Sphere**
    - **Validates: Requirements 16.5, 16.6**
  - [x] 12.10 Unit/example test for reveal-denied and reveal-failed responses
    - Assert denied response for no-permission reveal and failed response (no partial plaintext) for missing/corrupt ciphertext
    - _Requirements: 16.5, 16.8_

- [x] 13. Markdown pipeline: parse, sections, render
  - [x] 13.1 Implement goldmark parse with source-offset retention
    - Parse stored markdown to AST while retaining exact source byte ranges per section (heading-delimited); render AST→HTML for display
    - _Requirements: 11.1, 12.1_
  - [x] 13.2 Implement section view→edit round-trip
    - Serve the original stored bytes for a section's range verbatim on edit (never re-serialized from AST); cancel discards buffer leaving stored bytes untouched
    - _Requirements: 12.2, 12.3, 12.4_
  - [x] 13.3 Implement @-command handling
    - `@link` (edit-time selectable list of other Polygons in tenant; empty list + indicator when none), `@heading` (edit-time heading-style choices), `@toc` (render-time TOC from current headings; empty when none), `@children` (render-time list of direct child Polygons; empty when none); regenerate `@toc`/`@children` when headings or child set change; provide on-screen icons for each command
    - _Requirements: 11.2, 11.3, 11.4, 11.5, 11.6, 11.7, 11.8, 11.9, 11.10_
  - [x] 13.4 Implement external link validation and rendering
    - Store external links 1–2048 chars beginning with a supported web scheme; reject empty/oversize/bad-scheme leaving content unchanged with an invalid-URL indication; render stored external links as activatable anchors
    - _Requirements: 13.1, 13.2, 13.3_
  - [x] 13.5 Implement Cross-Refraction and ID-stable link resolution
    - Store links by target Record_ID; at render resolve to the target's current name and show activatable when it exists and is accessible; non-activatable "unavailable" when deleted; non-activatable "inaccessible" with name withheld when in an inaccessible Sphere; renames preserve links (same Record_ID) and resolve to updated name
    - _Requirements: 14.1, 14.2, 14.3, 14.4, 15.1, 15.3_
  - [x] 13.6 Implement the metadata footer
    - On create record author Central user id + creation ISO 8601 UTC timestamp; on edit update last-edit ISO 8601 UTC timestamp; render a footer with author display name (resolved from Central), creation date, and last-edit date as ISO 8601 UTC dates
    - _Requirements: 18.1, 18.2, 18.3_
  - [x] 13.7 Property test for section view→edit byte-for-byte round-trip
    - **Property 15: Section view→edit byte-for-byte round-trip**
    - **Validates: Requirements 12.3**
  - [x] 13.8 Property test for @toc reflecting current headings
    - **Property 13: `@toc` reflects current headings**
    - **Validates: Requirements 11.5, 11.6, 11.9**
  - [x] 13.9 Property test for @children reflecting current children
    - **Property 14: `@children` reflects current children**
    - **Validates: Requirements 11.7, 11.8, 11.9**
  - [x] 13.10 Property test for external link validation
    - **Property 12: External link validation**
    - **Validates: Requirements 13.1, 13.2**
  - [x] 13.11 Property test for ID-stable link resolution
    - **Property 10: ID-stable link resolution**
    - **Validates: Requirements 14.2, 15.1, 15.3**
  - [x] 13.12 Property test for Cross-Refraction outcome by target state
    - **Property 11: Cross-Refraction link outcome by target state**
    - **Validates: Requirements 14.2, 14.3, 14.4**
  - [x] 13.13 Property test for metadata footer content
    - **Property 23: Metadata footer content**
    - **Validates: Requirements 18.1, 18.2, 18.3**

- [x] 14. Image blob handling
  - [x] 14.1 Implement image paste storage and validation
    - Sniff actual content type; accept PNG/JPEG/GIF/WebP ≤ 10 MB, store as a blob in IMAGE_BLOB with a new image_id, insert an `influence://image/{image_id}` reference; reject unsupported format or oversize leaving content unchanged with the specific error; render resolves the reference to the served blob, placeholder when unresolvable
    - _Requirements: 17.1, 17.2, 17.3, 17.4, 17.5_
  - [x] 14.2 Property test for image paste validation
    - **Property 22: Image paste validation**
    - **Validates: Requirements 17.1, 17.2, 17.3**
  - [x] 14.3 Unit/example test for image rejection messages
    - Assert distinct unsupported-format vs too-large error messages
    - _Requirements: 17.2, 17.3_

- [x] 15. Checkpoint - Ensure all tests pass
  - Ensure all tests pass, ask the user if questions arise.

- [x] 16. Search (FTS5 over masked content) and quick searches
  - [x] 16.1 Implement FTS5 indexing over masked content
    - Populate `polygon_fts` from Masker-produced masked content (no plaintext, no ciphertext); keep the index in sync on Polygon create/edit/delete
    - _Requirements: 23.2, 16.7_
  - [x] 16.2 Implement search query scoping
    - Return only Polygons matching the query and residing in Spheres the user can access; empty result set when nothing matches
    - _Requirements: 23.1, 23.3_
  - [x] 16.3 Implement quick searches
    - "Polygons I've Created": authored by user, accessible Spheres, `created_at DESC`. "Recently Viewed Polygons": per-user VIEW_LOG, accessible Spheres, distinct, newest first, capped at 50. Empty result sets when nothing matches
    - _Requirements: 24.1, 24.2, 24.3_
  - [x] 16.4 Property test for search scoping and matching
    - **Property 24: Search scoping and matching**
    - **Validates: Requirements 23.1**
  - [x] 16.5 Property test for "Polygons I've Created" filter and order
    - **Property 25: "Polygons I've Created" filter and order**
    - **Validates: Requirements 24.1**
  - [x] 16.6 Property test for "Recently Viewed Polygons" cap and order
    - **Property 26: "Recently Viewed Polygons" cap and order**
    - **Validates: Requirements 24.2**
  - [x] 16.7 Integration test for FTS5 search wiring
    - End-to-end index+query against a temp tenant DB
    - _Requirements: 23.1_

- [x] 17. PDF export
  - [x] 17.1 Implement PDF export reusing the render path
    - Render the target Polygon through the same render route for the requesting user's context into a print-only layout omitting toolbars/menus; convert HTML to PDF via a headless renderer; masking inherited from the Masker (no-reveal user gets placeholders); on failure produce no file and return an error
    - _Requirements: 22.1, 22.2, 22.3_
  - [x] 17.2 Integration test for PDF reusing render path
    - Assert generated PDF text omits toolbars/menus and contains masked placeholders for a no-reveal user (mock the external headless renderer)
    - _Requirements: 22.1, 22.2_

- [x] 18. Concurrent editing
  - [x] 18.1 Implement WebSocket hub and per-Polygon mode
    - Each Polygon operates in exactly one mode (collaborative or record-locking) from `POLYGON.edit_mode`; stand up the WS hub with per-Polygon rooms for edit/lock signaling
    - _Requirements: 25.1_
  - [x] 18.2 Implement record-locking mode
    - Acquire EDIT_LOCK when a user begins editing; reject other users' saves with the holder's identity; release on explicit release, 300 s holder inactivity (`last_activity_at`), or holder disconnect
    - _Requirements: 25.3, 25.4, 25.5_
  - [x] 18.3 Property test for record lock exclusivity
    - **Property 27: Record lock exclusivity**
    - **Validates: Requirements 25.3, 25.4**
  - [x] 18.4 Implement collaborative CRDT mode (optional/advanced)
    - Represent edits as CRDT (RGA/Yjs-style sequence) over WebSocket; broadcast local changes to other editors, converging within 5 s; periodically snapshot converged document to POLYGON.content. This is the advanced path; the minimal viable product ships record-locking only
    - _Requirements: 25.2_
  - [x] 18.5 Integration test for collaborative propagation within 5 s (optional/advanced)
    - Two WS clients; assert an edit propagates to the other within the 5-second bound
    - _Requirements: 25.2_

- [x] 19. Bookmarks
  - [x] 19.1 Implement bookmark add/remove and grouping
    - Add is idempotent (already-bookmarked leaves list unchanged); remove deletes from the list; bookmarks menu lists bookmarked Polygons grouped by owning Sphere; empty-bookmarks indicator when none
    - _Requirements: 26.1, 26.2, 26.3, 26.4, 26.5_
  - [x] 19.2 Property test for bookmark idempotence and add/remove round-trip
    - **Property 28: Bookmark idempotence and add/remove round-trip**
    - **Validates: Requirements 26.1, 26.2, 26.5**
  - [x] 19.3 Property test for bookmarks grouped by Sphere
    - **Property 29: Bookmarks grouped by Sphere**
    - **Validates: Requirements 26.3**

- [x] 20. Tenant creation and export
  - [x] 20.1 Implement transactional tenant creation
    - Saga: insert a `pending` registry row in Central_Directory; create + migrate the new tenant SQLite file (also initializing TENANT_SECRET per task 12.1); mark `active`. On file-creation failure, roll back the registry row and return an error; no partial tenant remains
    - _Requirements: 1.7, 1.8_
  - [x] 20.2 Implement tenant export
    - Produce a consistent, self-sufficient copy of the tenant DB (SQLite online-backup / `VACUUM INTO`) including Tenant_Salt + wrapped key material; abort with an error if the file is missing, locked, or corrupt
    - _Requirements: 2.2, 2.4, 16.9_
  - [x] 20.3 Property test for encrypted content surviving export/import
    - **Property 21: Encrypted content survives tenant export/import**
    - **Validates: Requirements 2.2, 16.9**
  - [x] 20.4 Integration tests for tenant create rollback and export snapshot
    - Assert DB-file-creation failure rolls back the registry row (Req 1.8); assert export aborts on missing/locked/corrupt file (Req 2.4)
    - _Requirements: 1.8, 2.4_

- [x] 21. Checkpoint - Ensure all tests pass
  - Ensure all tests pass, ask the user if questions arise.

- [x] 22. HTTP API surface and error envelope
  - [x] 22.1 Wire REST handlers for all services
    - Implement the routes from the API table (auth, spheres, circles, facets, polygons, render, encrypt, reveal, images, pdf, search, quick, lock, bookmarks) behind the middleware chain; thin handlers delegating to services
    - _Requirements: 3.1, 3.5, 6.2, 6.5, 7.1, 7.4, 7.6, 8.4, 9.1, 10.2, 12.1, 16.1, 16.4, 17.1, 17.4, 22.1, 23.1, 24.1, 24.2, 25.3, 26.1_
  - [x] 22.2 Implement the standard error envelope
    - `{ "error": { "code", "message" } }` with codes TENANT_ISOLATION, VALIDATION, HIERARCHY, REVEAL_DENIED, REVEAL_FAILED, LOCKED, ADMIN_REQUIRED; mutating operations run in transactions leaving state unchanged on failure
    - _Requirements: 1.6, 9.1, 10.6, 13.2, 16.5, 16.8, 25.4, 5.5_

- [x] 23. Frontend: navigation, editor, views, admin
  - [x] 23.1 Implement the app shell and left-menu navigation
    - Top bar (search, quick searches, bookmarks menu, user menu) + left menu listing only accessible Spheres ordered per Req 7 (circled group reorderable above alphabetical remainder); selecting a Sphere expands its Facet/Polygon tree; empty-contents indicator for empty Spheres; routes built from Short-UUID Record_IDs
    - _Requirements: 21.1, 21.2, 21.3, 7.3, 7.5, 15.2_
  - [x] 23.2 Implement the markdown editor and Polygon views
    - Section-based rich view + editor, `@`-command menu with toolbar icons, image-paste handler, encrypt-selection action, per-token reveal control; type-specific views (MarkdownPage, FolderPolygon with `@children`, TabularPolygon grid, WhiteboardPolygon canvas)
    - _Requirements: 11.10, 12.1, 12.2, 16.4, 17.1, 10.1_
  - [x] 23.3 Implement admin, bookmarks, and docs UI
    - Admin section (Users, Groups, per-Sphere access matrix with access level + Reveal) visible only to Admin_Group; bookmarks menu grouped by Sphere with empty-state; in-app docs viewer with unavailable-state
    - _Requirements: 5.1, 5.2, 5.3, 26.3, 26.4, 27.2, 27.3_
  - [x] 23.4 Frontend component/example tests for navigation and empty states
    - Assert the left menu renders circled Spheres above the alphabetical remainder, shows the empty-contents indicator for an empty Sphere, and builds routes from Short-UUID Record_IDs (the universal ordering invariant is covered by the backend Property 5 test)
    - _Requirements: 21.1, 21.2, 21.3, 15.2_

- [x] 24. Documentation
  - [x] 24.1 Author platform documentation and in-app rendering
    - Maintain platform docs as unmodified markdown files under `docs/` suitable for GitHub; serve them via `GET /api/docs/{path}` and render in-app; show an unavailable indication when a doc cannot be loaded
    - _Requirements: 27.1, 27.2, 27.3, 27.4_

- [x] 25. Build, deploy, and packaging
  - [x] 25.1 Create the Makefile targets
    - `dev` (foreground run, no systemd), `build` (single FTS5-enabled static binary; exit 0 on success, non-zero + error on failure), `install` (copy binary, create non-login `influence` service user, create/own `/var/lib/influence`, install default config only if absent, install+enable systemd unit; idempotent), `uninstall` (stop+disable+remove service and binary; preserve Data_Directory unless `PURGE_DATA=1`; idempotent)
    - _Requirements: 28.1, 28.2, 28.3, 28.4, 28.5, 28.6, 28.7_
  - [x] 25.2 Create the systemd unit and OS packaging
    - `deploy/influence.service` running as the non-login `influence` user with hardening and `ReadWritePaths=/var/lib/influence`; `.deb` and `.rpm` manifests for Ubuntu 20.04+ and Fedora 38+
    - _Requirements: 20.1, 20.2, 28.4_
  - [x] 25.3 Integration tests for install/uninstall idempotence and config/startup validation
    - install→uninstall→re-run idempotence and Data_Directory preservation in a systemd-capable container/sandbox; config/port/data-dir startup validation (invalid/out-of-range port makes no bind attempt, port-in-use fails, missing/non-writable Data_Directory fails)
    - _Requirements: 28.4, 28.5, 28.6, 28.7, 29.3, 29.4, 30.4, 30.5_
  - [x] 25.4 Property test for install/uninstall idempotence
    - **Property 32: Install/uninstall idempotence**
    - **Validates: Requirements 28.4, 28.5, 28.6, 28.7**

- [x] 26. Final checkpoint - Ensure all tests pass
  - Ensure all tests pass, ask the user if questions arise.

- [x] 27. Admin & Identity API surface
  - [x] 27.1 Implement the identity endpoint
    - Add `GET /api/me` returning the caller's user id, display name, Admin_Group membership, 2FA enrolment status, and the tenant 2FA policy, read from the RequestContext plus a Central lookup; available to any authenticated session
    - _Requirements: 32.1_
  - [x] 27.2 Wire admin user-lifecycle routes
    - Behind the existing `RequireAdmin` guard: `GET/POST /api/admin/users` (list/create with shared password policy, add to a default Group to satisfy Req 4.1, transactional rollback on failure) and `POST /api/admin/users/{id}/deactivate|reactivate` toggling `USER.deactivated`; tenant-scoped from the RequestContext
    - _Requirements: 5.1, 5.4, 32.2, 32.6, 32.7_
  - [x] 27.3 Wire admin group + membership routes
    - `GET/POST /api/admin/groups`, `POST /api/admin/groups/{id}/members`, `DELETE .../members/{userId}` delegating to `UserGroupService.AddMembership`/`RemoveMembership`; surface the min-one-Group invariant as a `VALIDATION` error
    - _Requirements: 5.2, 32.3_
  - [x] 27.4 Wire admin per-Sphere access matrix routes
    - `GET /api/admin/sphere-access`, `PUT` / `DELETE /api/admin/sphere-access` delegating to `GrantSphere`/`RevokeSphere` (level + Reveal), tenant-scoped
    - _Requirements: 5.3, 32.4_
  - [x] 27.5 Property test for Admin API scoping and admin-only access
    - **Property 34: Admin API tenant scoping and admin-only access**
    - **Validates: Requirements 32.2, 32.3, 32.4, 32.5, 32.6, 32.7**
  - [x] 27.6 Example tests for admin-denial and tenant-isolation responses
    - Assert a non-admin caller receives `ADMIN_REQUIRED` and mutates nothing, and a cross-tenant reference is rejected with `TENANT_ISOLATION`, for each admin route family
    - _Requirements: 5.5, 32.5, 32.6_

- [x] 28. First-run administrator bootstrap
  - [x] 28.1 Implement the shared password policy validator
    - Single validator (minimum length + basic strength) reused by bootstrap, admin user-creation, and future password changes
    - _Requirements: 31.2, 31.5, 32.2_
  - [x] 28.2 Implement the BootstrapService and first-run endpoints
    - Add `GET /api/setup/state` (reports First_Run_State, derived from "no Admin_Group member for the tenant") and `POST /api/setup/admin` (single Central transaction: re-check First_Run_State, validate username 1–100 chars + password policy, Argon2id hash, insert USER, ensure Admin_Group, insert membership); reject with `SETUP_COMPLETE` once any admin exists; deny the setup endpoint when not in First_Run_State
    - _Requirements: 31.1, 31.2, 31.3, 31.4, 31.6_
  - [x] 28.3 Render the setup screen in the frontend shell
    - Shell queries `GET /api/setup/state` on load; while First_Run_State is true, render the Setup_Screen (username + password) in place of login, post to `/api/setup/admin`, then reload into normal login
    - _Requirements: 31.1, 31.3_
  - [x] 28.4 Property test for first-run bootstrap idempotence and exclusivity
    - **Property 33: First-run bootstrap idempotence and exclusivity**
    - **Validates: Requirements 31.2, 31.3, 31.4, 31.6**

- [x] 29. Two-factor authentication
  - [x] 29.1 Add the 2FA schema migration
    - Additive Central changes: `USER.two_factor_secret`, `USER.two_factor_enrolled`, `USER.last_totp_step`; new `RECOVERY_CODE` and `TWO_FACTOR_POLICY` tables; `SESSION.state` (`active|pending_2fa|must_enrol`); all defaulted for backward-compatible migration
    - _Requirements: 33.1, 33.2, 33.6, 33.7_
  - [x] 29.2 Implement TOTP enrolment with QR provisioning
    - `POST /api/2fa/enrol` generates a TOTP_Secret (stored pending) and returns the base32 secret + `otpauth://` provisioning URI; `POST /api/2fa/confirm` verifies a TOTP against the pending secret, marks enrolled, and issues single-use Recovery_Codes stored as Argon2id hashes; use an established Go TOTP library, not a from-scratch implementation
    - _Requirements: 33.1, 33.2, 33.3_
  - [x] 29.3 Integrate the second factor into the login flow
    - After password verification, require a valid TOTP (current step ±1 for drift, rejecting a replayed step via `last_totp_step`) or an unused Recovery_Code (invalidated on use) before establishing an `active` session; feed repeated second-factor failures into the existing Req 3.3 lockout; return distinct `202` states for second-factor-required vs enrolment-required
    - _Requirements: 33.4, 33.5, 33.6, 33.9_
  - [x] 29.4 Implement the required-policy gate and admin toggle
    - `PUT /api/admin/2fa-policy` (admin) sets the tenant policy optional/required; when required and the user is unenrolled, a correct password yields a `must_enrol` restricted session the middleware limits to the enrolment + logout routes (all others `ENROLMENT_REQUIRED`)
    - _Requirements: 33.7, 33.8_
  - [x] 29.5 Build the frontend 2FA enrolment and login views
    - Enrolment view renders the provisioning URI as a QR (client-side) + secret text + confirm-code field and displays the issued Recovery_Codes; login prompts for a TOTP/recovery code when enrolled; a `must_enrol` session lands directly on enrolment; admin gets the optional/required toggle
    - _Requirements: 33.1, 33.4, 33.7, 33.8_
  - [x] 29.6 Property test for the 2FA gate and single-use recovery codes
    - **Property 35: Two-factor authentication gate and single-use recovery codes**
    - **Validates: Requirements 33.4, 33.5, 33.6, 33.7, 33.9**

- [x] 30. Final checkpoint - Ensure all new tests pass
  - Run the full backend (`go build ./... && go vet ./... && go test ./... -count=1`) and frontend (`npm run check && npm run test && npm run build`) suites; ensure all tests pass, ask the user if questions arise.

## Notes

- Tasks marked with `*` are optional test sub-tasks and can be skipped for a faster MVP; core implementation sub-tasks are never optional.
- Each task references specific requirement sub-clauses for traceability.
- Property tests use `pgregory.net/rapid` (Go) and `fast-check` (frontend), one test per Correctness Property, minimum 100 iterations, tagged `// Feature: influence, Property {n}: {property text}`. Do not implement PBT from scratch.
- **Property 18 (masking never leaks plaintext or ciphertext) is the highest-priority security test** and must assert absence of both plaintext and ciphertext across render, PDF-text, and search-index outputs.
- Repository-level property tests use a real in-memory/temp SQLite tenant DB; only the external headless-PDF renderer is mocked.
- The collaborative CRDT mode (tasks 18.4, 18.5) is the advanced path; a minimal viable product can ship with record-locking only.
- Checkpoints ensure incremental validation; multiple checkpoints are included.

## Task Dependency Graph

```json
{
  "waves": [
    { "id": 0, "tasks": ["1.1", "1.3"] },
    { "id": 1, "tasks": ["1.2", "2.1", "3.1", "3.2"] },
    { "id": 2, "tasks": ["2.2", "2.3", "3.3", "4.1"] },
    { "id": 3, "tasks": ["2.4", "2.5", "2.6", "3.4", "4.2", "5.1"] },
    { "id": 4, "tasks": ["4.3", "5.2", "5.3", "6.1"] },
    { "id": 5, "tasks": ["5.4", "6.2", "7.1"] },
    { "id": 6, "tasks": ["6.3", "6.4", "7.2", "8.1", "9.1", "11.1"] },
    { "id": 7, "tasks": ["8.2", "8.3", "9.2", "11.2", "12.1"] },
    { "id": 8, "tasks": ["8.4", "8.5", "9.3", "12.2", "12.3"] },
    { "id": 9, "tasks": ["12.4", "12.6", "12.7", "12.8", "12.9", "12.10", "13.1"] },
    { "id": 10, "tasks": ["12.5", "13.2", "13.3", "13.4", "13.5", "13.6", "14.1"] },
    { "id": 11, "tasks": ["13.7", "13.8", "13.9", "13.10", "13.11", "13.12", "13.13", "14.2", "14.3"] },
    { "id": 12, "tasks": ["16.1", "18.1", "20.1"] },
    { "id": 13, "tasks": ["16.2", "16.3", "17.1", "18.2", "19.1", "20.2"] },
    { "id": 14, "tasks": ["16.4", "16.5", "16.6", "16.7", "17.2", "18.3", "18.4", "19.2", "19.3", "20.3", "20.4"] },
    { "id": 15, "tasks": ["18.5", "22.1"] },
    { "id": 16, "tasks": ["22.2", "23.1", "23.2", "23.3", "24.1"] },
    { "id": 17, "tasks": ["23.4", "25.1"] },
    { "id": 18, "tasks": ["25.2", "25.3", "25.4"] },
    { "id": 19, "tasks": ["27.1", "27.2", "27.3", "27.4", "28.1", "29.1"] },
    { "id": 20, "tasks": ["27.5", "27.6", "28.2", "29.2", "29.4"] },
    { "id": 21, "tasks": ["28.3", "28.4", "29.3"] },
    { "id": 22, "tasks": ["29.5", "29.6"] },
    { "id": 23, "tasks": ["30"] }
  ]
}
```
