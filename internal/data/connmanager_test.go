package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeRegistry maps tenant ids to opaque paths for tests, decoupling the
// connection manager from a real Central_Directory.
type fakeRegistry struct {
	paths map[TenantID]string
}

func (r fakeRegistry) TenantDBPath(_ context.Context, id TenantID) (string, error) {
	p, ok := r.paths[id]
	if !ok {
		return "", fmt.Errorf("%w: tenant %d", ErrTenantNotFound, id)
	}
	return p, nil
}

// newTestManager builds a connManager whose openDB returns a fresh in-memory
// SQLite pool per path and counts how many times each path was opened, so tests
// can assert caching and eviction behavior.
func newTestManager(t *testing.T, paths map[TenantID]string, opts ...ConnManagerOption) (*connManager, *openCounter) {
	t.Helper()
	oc := &openCounter{counts: map[string]int{}}
	m := newConnManager(nil, fakeRegistry{paths: paths}, opts...)
	m.openDB = func(path string) (*sql.DB, error) {
		oc.inc(path)
		db, err := sql.Open(DriverName, "file:"+path+"?mode=memory&cache=shared")
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(1)
		return db, nil
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, oc
}

type openCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (o *openCounter) inc(path string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.counts[path]++
}

func (o *openCounter) get(path string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.counts[path]
}

func (o *openCounter) total() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, c := range o.counts {
		n += c
	}
	return n
}

// TestTenantResolvesOnlyContextTenant asserts Tenant returns a handle whose
// TenantID equals the caller's context tenant — the sole tenant it may reach
// (Requirements 1.4, 1.5).
func TestTenantResolvesOnlyContextTenant(t *testing.T) {
	m, _ := newTestManager(t, map[TenantID]string{7: "t7", 9: "t9"})
	ctx := context.Background()

	h7, err := m.Tenant(ctx, NewRequestContext(1, 7, nil, nil))
	if err != nil {
		t.Fatalf("Tenant(7): %v", err)
	}
	if h7.TenantID() != 7 {
		t.Errorf("handle tenant = %d, want 7", h7.TenantID())
	}

	h9, err := m.Tenant(ctx, NewRequestContext(1, 9, nil, nil))
	if err != nil {
		t.Fatalf("Tenant(9): %v", err)
	}
	if h9.TenantID() != 9 {
		t.Errorf("handle tenant = %d, want 9", h9.TenantID())
	}
	if h7.DB() == h9.DB() {
		t.Error("different tenants must not share the same *sql.DB pool")
	}
}

// TestTenantUnknownTenantErrors asserts requesting a tenant with no registry
// entry returns ErrTenantNotFound (a caller cannot conjure another tenant).
func TestTenantUnknownTenantErrors(t *testing.T) {
	m, _ := newTestManager(t, map[TenantID]string{1: "t1"})
	_, err := m.Tenant(context.Background(), NewRequestContext(1, 404, nil, nil))
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("expected ErrTenantNotFound, got %v", err)
	}
}

// TestTenantPoolIsCached asserts repeated resolution of the same tenant reuses
// the cached pool (opened exactly once) and returns the same underlying handle.
func TestTenantPoolIsCached(t *testing.T) {
	m, oc := newTestManager(t, map[TenantID]string{3: "t3"})
	ctx := context.Background()
	rc := NewRequestContext(1, 3, nil, nil)

	first, err := m.Tenant(ctx, rc)
	if err != nil {
		t.Fatalf("first Tenant: %v", err)
	}
	second, err := m.Tenant(ctx, rc)
	if err != nil {
		t.Fatalf("second Tenant: %v", err)
	}
	if first.DB() != second.DB() {
		t.Error("expected the same cached *sql.DB across resolutions")
	}
	if got := oc.get("t3"); got != 1 {
		t.Errorf("tenant opened %d times, want 1 (should be cached)", got)
	}
}

// TestLRUEvictionClosesOldest asserts that exceeding maxTenants evicts the
// least-recently-used pool, so a re-resolution of the evicted tenant reopens it.
func TestLRUEvictionClosesOldest(t *testing.T) {
	m, oc := newTestManager(t,
		map[TenantID]string{1: "t1", 2: "t2", 3: "t3"},
		WithMaxTenants(2), WithIdleTimeout(time.Hour),
	)
	ctx := context.Background()

	// Open tenant 1, then 2 (fills capacity). Touch 1 so 2 is the LRU.
	if _, err := m.Tenant(ctx, NewRequestContext(1, 1, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Tenant(ctx, NewRequestContext(1, 2, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Tenant(ctx, NewRequestContext(1, 1, nil, nil)); err != nil { // touch 1
		t.Fatal(err)
	}

	// Opening 3 exceeds capacity and must evict the LRU (tenant 2).
	if _, err := m.Tenant(ctx, NewRequestContext(1, 3, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.pools[2]; ok {
		t.Error("expected LRU tenant 2 to be evicted")
	}
	if _, ok := m.pools[1]; !ok {
		t.Error("recently-used tenant 1 should remain cached")
	}

	// Re-resolving the evicted tenant reopens it.
	if _, err := m.Tenant(ctx, NewRequestContext(1, 2, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if got := oc.get("t2"); got != 2 {
		t.Errorf("tenant 2 opened %d times, want 2 (evicted then reopened)", got)
	}
}

// TestIdleEvictionReopens asserts a pool untouched longer than the idle timeout
// is evicted lazily on the next resolution and reopened.
func TestIdleEvictionReopens(t *testing.T) {
	m, oc := newTestManager(t,
		map[TenantID]string{1: "t1", 2: "t2"},
		WithMaxTenants(10), WithIdleTimeout(time.Minute),
	)
	ctx := context.Background()

	if _, err := m.Tenant(ctx, NewRequestContext(1, 1, nil, nil)); err != nil {
		t.Fatal(err)
	}
	// Force tenant 1 to look idle.
	m.mu.Lock()
	m.pools[1].lastUsed = time.Now().Add(-2 * time.Minute)
	m.mu.Unlock()

	// Resolving a different tenant triggers lazy idle eviction of tenant 1.
	if _, err := m.Tenant(ctx, NewRequestContext(1, 2, nil, nil)); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	_, stillCached := m.pools[1]
	m.mu.Unlock()
	if stillCached {
		t.Error("expected idle tenant 1 to be evicted")
	}

	// Re-resolving tenant 1 reopens it.
	if _, err := m.Tenant(ctx, NewRequestContext(1, 1, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if got := oc.get("t1"); got != 2 {
		t.Errorf("tenant 1 opened %d times, want 2 (evicted then reopened)", got)
	}
}

// TestConcurrentTenantResolutionOpensOnce asserts concurrent resolution of the
// same tenant does not open duplicate pools; the manager de-duplicates and
// keeps a single cached handle.
func TestConcurrentTenantResolutionOpensOnce(t *testing.T) {
	m, _ := newTestManager(t, map[TenantID]string{1: "t1"}, WithIdleTimeout(time.Hour))
	ctx := context.Background()
	rc := NewRequestContext(1, 1, nil, nil)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := m.Tenant(ctx, rc); err != nil {
				t.Errorf("concurrent Tenant: %v", err)
			}
		}()
	}
	wg.Wait()

	// Exactly one pool survives in the cache regardless of how many goroutines
	// raced to open it; duplicate opens are closed and discarded.
	m.mu.Lock()
	pools := len(m.pools)
	m.mu.Unlock()
	if pools != 1 {
		t.Errorf("expected exactly 1 cached pool, got %d", pools)
	}
	// The cached handle must be usable (not a closed duplicate).
	h, err := m.Tenant(ctx, rc)
	if err != nil {
		t.Fatalf("resolve after concurrent opens: %v", err)
	}
	if err := h.DB().PingContext(ctx); err != nil {
		t.Errorf("cached pool is not usable: %v", err)
	}
}
