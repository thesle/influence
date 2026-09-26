package data

// The connection manager is the most security-critical component in the data
// layer (design.md — "Connection Manager (Tenant Routing)", Requirements 1.4,
// 1.5). It owns one long-lived handle to the Central_Directory and a bounded,
// lazily-populated map of per-tenant *sql.DB pools.
//
// The isolation guarantee is structural: Tenant resolves a handle ONLY for the
// tenant named in the caller's RequestContext. There is deliberately no exported
// method that opens an arbitrary tenant by id, so a service — which always holds
// a RequestContext — can never reach another tenant's file to satisfy a content
// request. A record id absent from the caller's tenant simply isn't found, and
// the service returns "not accessible" (Requirement 1.6) with no cross-tenant
// query path.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// TenantDB is the scoped handle a service receives for its own tenant. It wraps
// the tenant's *sql.DB and carries the TenantID it belongs to. It is the sole
// gateway to a tenant's content; a service cannot obtain one for any tenant
// other than the one in its RequestContext.
type TenantDB struct {
	tenantID TenantID
	db       *sql.DB
}

// TenantID reports which tenant this handle belongs to.
func (t *TenantDB) TenantID() TenantID { return t.tenantID }

// DB returns the underlying database handle for the tenant. Repositories run
// their tenant-scoped queries against it.
func (t *TenantDB) DB() *sql.DB { return t.db }

// TenantRegistry resolves a TenantID to the filesystem path of that tenant's
// SQLite database. It is backed by the Central_Directory TENANT table. It is an
// interface so the connection manager can be tested without a full central
// database and so tenant-creation logic (a later task) can supply the mapping.
type TenantRegistry interface {
	// TenantDBPath returns the SQLite file path for the given tenant, or an
	// error if the tenant is unknown or not yet active.
	TenantDBPath(ctx context.Context, id TenantID) (string, error)
}

// centralTenantRegistry resolves tenant db paths from the Central_Directory
// TENANT table (db_path column), restricted to tenants in the 'active' state so
// a half-created tenant (status 'pending', Requirement 1.7/1.8) is never opened.
type centralTenantRegistry struct {
	central *sql.DB
}

// TenantDBPath looks up the db_path for an active tenant.
func (r centralTenantRegistry) TenantDBPath(ctx context.Context, id TenantID) (string, error) {
	var path string
	err := r.central.QueryRowContext(ctx,
		`SELECT db_path FROM "TENANT" WHERE id = ? AND status = 'active'`, int64(id),
	).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: tenant %d", ErrTenantNotFound, id)
	}
	if err != nil {
		return "", fmt.Errorf("resolve tenant %d db path: %w", id, err)
	}
	return path, nil
}

// ErrTenantNotFound is returned when a requested tenant has no active registry
// entry. Callers translate this into a "not accessible" response (Req 1.6)
// rather than leaking whether another tenant exists.
var ErrTenantNotFound = errors.New("tenant not found")

// tenantEntry is one cached tenant pool plus its last-use timestamp for
// idle-eviction bookkeeping.
type tenantEntry struct {
	db       *sql.DB
	lastUsed time.Time
}

// ConnManager is the interface the service layer depends on. Central hands out
// the shared identity database; Tenant hands out the caller's own tenant handle
// and nothing else. CreateTenant and ExportTenant are declared here for the
// full contract (design.md) but implemented in later tasks (20.1, 20.2).
type ConnManager interface {
	// Central returns the shared Central_Directory handle (identity + registry).
	Central() *sql.DB
	// Tenant returns a handle ONLY for ctx.TenantID. It is the sole gateway to
	// tenant content; there is no way to open an arbitrary tenant.
	Tenant(ctx context.Context, rc RequestContext) (*TenantDB, error)
	// Close closes every open pool, including the central handle.
	Close() error
}

// connManager is the concrete ConnManager. It keeps one central handle and a
// bounded LRU/idle-evicting map of per-tenant pools so thousands of tenants do
// not exhaust file handles (design.md — "with an LRU/idle-eviction bound").
type connManager struct {
	central  *sql.DB
	registry TenantRegistry

	// openDB opens (and prepares) a tenant pool for a resolved db path. It is a
	// field so tests can substitute in-memory databases.
	openDB func(path string) (*sql.DB, error)

	maxTenants  int           // LRU capacity: most pools kept open at once.
	idleTimeout time.Duration // pools untouched this long are evicted lazily.

	mu    sync.Mutex
	pools map[TenantID]*tenantEntry
}

// ConnManagerOption customizes eviction bounds. Sensible defaults apply when no
// options are given.
type ConnManagerOption func(*connManager)

// WithMaxTenants sets the maximum number of tenant pools kept open at once.
// When exceeded, the least-recently-used pool is closed and evicted. A value
// <= 0 leaves the default in place.
func WithMaxTenants(n int) ConnManagerOption {
	return func(m *connManager) {
		if n > 0 {
			m.maxTenants = n
		}
	}
}

// WithIdleTimeout sets how long a tenant pool may sit unused before it is
// eligible for lazy eviction. A value <= 0 leaves the default in place.
func WithIdleTimeout(d time.Duration) ConnManagerOption {
	return func(m *connManager) {
		if d > 0 {
			m.idleTimeout = d
		}
	}
}

const (
	defaultMaxTenants  = 64
	defaultIdleTimeout = 5 * time.Minute
)

// NewConnManager builds a ConnManager over an already-open Central_Directory
// handle. Tenant db paths are resolved from the central TENANT registry. Tenant
// pools are opened lazily on first use and evicted by LRU/idle bounds.
func NewConnManager(central *sql.DB, opts ...ConnManagerOption) ConnManager {
	return newConnManager(central, centralTenantRegistry{central: central}, opts...)
}

// newConnManager is the internal constructor that accepts an explicit registry,
// enabling tests to inject a fake tenant→path mapping without a full central DB.
func newConnManager(central *sql.DB, registry TenantRegistry, opts ...ConnManagerOption) *connManager {
	m := &connManager{
		central:     central,
		registry:    registry,
		openDB:      openTenantPool,
		maxTenants:  defaultMaxTenants,
		idleTimeout: defaultIdleTimeout,
		pools:       make(map[TenantID]*tenantEntry),
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// openTenantPool opens a tenant SQLite pool with foreign keys enabled so the
// schema's ON DELETE CASCADE rules hold. It does not migrate; tenant files are
// migrated at creation time (task 20.1).
func openTenantPool(path string) (*sql.DB, error) {
	db, err := sql.Open(DriverName, path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	return db, nil
}

// Central returns the shared Central_Directory handle.
func (m *connManager) Central() *sql.DB { return m.central }

// Tenant returns the handle for the caller's authenticated tenant — and no
// other. The tenant id comes from the RequestContext, never from client input,
// so a caller structurally cannot reach a tenant it was not authenticated for
// (Requirements 1.4, 1.5). The pool is opened lazily on first use and cached
// with LRU/idle eviction bounds.
func (m *connManager) Tenant(ctx context.Context, rc RequestContext) (*TenantDB, error) {
	id := rc.TenantID()

	m.mu.Lock()
	if e, ok := m.pools[id]; ok {
		e.lastUsed = time.Now()
		db := e.db
		m.mu.Unlock()
		return &TenantDB{tenantID: id, db: db}, nil
	}
	m.mu.Unlock()

	// Resolve the path and open outside the lock (open may touch the disk).
	path, err := m.registry.TenantDBPath(ctx, id)
	if err != nil {
		return nil, err
	}
	db, err := m.openDB(path)
	if err != nil {
		return nil, fmt.Errorf("open tenant %d database: %w", id, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Another goroutine may have opened the same tenant while we were opening;
	// prefer the existing entry and discard our duplicate.
	if e, ok := m.pools[id]; ok {
		_ = db.Close()
		e.lastUsed = time.Now()
		return &TenantDB{tenantID: id, db: e.db}, nil
	}

	m.evictIfNeededLocked()
	m.pools[id] = &tenantEntry{db: db, lastUsed: time.Now()}
	return &TenantDB{tenantID: id, db: db}, nil
}

// evictIfNeededLocked enforces both eviction bounds. It must be called with
// m.mu held. It first drops any pool idle past idleTimeout, then, if the map is
// still at or over capacity, closes the single least-recently-used pool.
func (m *connManager) evictIfNeededLocked() {
	now := time.Now()

	// Idle eviction: close anything untouched for longer than idleTimeout.
	for id, e := range m.pools {
		if now.Sub(e.lastUsed) >= m.idleTimeout {
			_ = e.db.Close()
			delete(m.pools, id)
		}
	}

	// LRU eviction: if still full, evict the oldest single entry to make room
	// for the incoming pool.
	if len(m.pools) < m.maxTenants {
		return
	}
	var oldestID TenantID
	var oldest time.Time
	first := true
	for id, e := range m.pools {
		if first || e.lastUsed.Before(oldest) {
			oldestID = id
			oldest = e.lastUsed
			first = false
		}
	}
	if !first {
		_ = m.pools[oldestID].db.Close()
		delete(m.pools, oldestID)
	}
}

// Close closes every open tenant pool and the central handle. It is safe to
// call once at shutdown.
func (m *connManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	for id, e := range m.pools {
		if err := e.db.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close tenant %d: %w", id, err))
		}
		delete(m.pools, id)
	}
	if m.central != nil {
		if err := m.central.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close central: %w", err))
		}
	}
	return errors.Join(errs...)
}
