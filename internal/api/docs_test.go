package api

// Tests for the in-app documentation route GET /api/docs/{path} (Req 27).
//
// These tests do not need the full wired server or the authenticated chain —
// the docs route is unauthenticated and depends only on the injected docsDir —
// so they build a bare Server, point it at a temp docs directory via WithDocsDir,
// and drive requests through the real chi router with httptest. They cover the
// three behaviors the task requires: an existing doc is served, a path-traversal
// attempt is rejected, and a missing doc returns the unavailable indication.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// newDocsServer builds a Server whose docsDir is a fresh temp directory seeded
// with the given files, and returns the server plus the temp dir. Only the
// fields the docs route touches are set, so no database wiring is needed.
func newDocsServer(t *testing.T, files map[string]string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %q: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	srv := (&Server{}).WithDocsDir(dir)
	return srv, dir
}

// docsRouter mounts just the docs route so the tests exercise real chi wildcard
// routing without standing up the authenticated chain.
func docsRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/api/docs/*", s.handleGetDoc)
	return r
}

func TestHandleGetDoc_ServesExistingDoc(t *testing.T) {
	const body = "# Getting Started\n\nWelcome to Influence.\n"
	srv, _ := newDocsServer(t, map[string]string{"getting-started.md": body})
	router := docsRouter(srv)

	t.Run("raw markdown by default", func(t *testing.T) {
		rr := doDocsGet(router, "/api/docs/getting-started.md")
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		if got := rr.Body.String(); got != body {
			t.Errorf("body = %q, want unmodified markdown %q", got, body)
		}
		if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/markdown") {
			t.Errorf("Content-Type = %q, want text/markdown", ct)
		}
	})

	t.Run("rendered html on request", func(t *testing.T) {
		rr := doDocsGet(router, "/api/docs/getting-started.md?format=html")
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", ct)
		}
		if !strings.Contains(rr.Body.String(), "<h1") {
			t.Errorf("html body missing rendered heading: %q", rr.Body.String())
		}
	})
}

func TestHandleGetDoc_ServesNestedDoc(t *testing.T) {
	srv, _ := newDocsServer(t, map[string]string{"guide/intro.md": "# Intro\n"})
	router := docsRouter(srv)

	rr := doDocsGet(router, "/api/docs/guide/intro.md")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "# Intro") {
		t.Errorf("body = %q, want nested doc content", rr.Body.String())
	}
}

func TestHandleGetDoc_RejectsPathTraversal(t *testing.T) {
	// Seed a secret file OUTSIDE the docs dir that a traversal attempt would
	// target, to prove it is never served.
	srv, docsDir := newDocsServer(t, map[string]string{"readme.md": "# Docs\n"})
	parent := filepath.Dir(docsDir)
	secretPath := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	router := docsRouter(srv)

	// A ".." segment in the request path must be rejected, not resolved.
	for _, target := range []string{
		"/api/docs/../secret.txt",
		"/api/docs/nested/../../secret.txt",
	} {
		rr := doDocsGet(router, target)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 (traversal rejected); body=%s", target, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "SECRET") {
			t.Fatalf("%s: response leaked the out-of-tree secret: %s", target, rr.Body.String())
		}
	}
}

func TestHandleGetDoc_MissingDocIsUnavailable(t *testing.T) {
	srv, _ := newDocsServer(t, map[string]string{"readme.md": "# Docs\n"})
	router := docsRouter(srv)

	rr := doDocsGet(router, "/api/docs/does-not-exist.md")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	// The unavailable indication is the standard error envelope with NOT_FOUND.
	body := rr.Body.String()
	if !strings.Contains(body, codeNotFound) {
		t.Errorf("body = %q, want standard error envelope with %q", body, codeNotFound)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json error envelope", ct)
	}
}

// TestResolveDocPath_Guards exercises the path-confinement logic directly, so
// the traversal defense is verified independent of any router path cleaning.
func TestResolveDocPath_Guards(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := (&Server{}).WithDocsDir(dir)

	rejected := []string{
		"",                   // empty
		"..",                 // bare traversal
		"../secret.txt",      // parent escape
		"a/../../secret.txt", // nested escape
		"/etc/passwd",        // absolute
	}
	for _, in := range rejected {
		if _, ok := s.resolveDocPath(in); ok {
			t.Errorf("resolveDocPath(%q) = ok, want rejected", in)
		}
	}

	if got, ok := s.resolveDocPath("ok.md"); !ok {
		t.Errorf("resolveDocPath(%q) rejected, want confined path", "ok.md")
	} else if base := filepath.Base(got); base != "ok.md" {
		t.Errorf("resolveDocPath(%q) = %q, want a path ending in ok.md", "ok.md", got)
	}
}

// doDocsGet performs a GET through the router and returns the recorder.
func doDocsGet(router http.Handler, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}
