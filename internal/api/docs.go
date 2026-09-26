package api

// In-app documentation serving (design.md — "GET /api/docs/{path}", Req 27).
//
// The platform documentation is authored as plain, unmodified markdown files
// under the docs/ directory so the very same files render on GitHub and inside
// the Influence web application (Req 27.1, 27.4). This handler reads the
// requested file from that directory and serves it: the raw markdown by default,
// or server-rendered HTML when the caller asks for it, so the frontend can
// render the documentation without shipping a markdown engine of its own (Req
// 27.2, 27.3).
//
// SECURITY. The {path} is caller-controlled, so the handler must never let it
// escape the docs directory. Every request is confined to docsDir: the cleaned,
// resolved target must stay inside the resolved docs root, and any traversal
// attempt (".." segments, absolute paths, symlinks pointing outside) is rejected
// as not-found rather than reaching the filesystem outside docs/.
//
// UNAVAILABILITY. When the requested document is missing or cannot be read, the
// handler returns the standard error envelope with 404 NOT_FOUND so the web
// application can show a clear "documentation could not be loaded" indication
// (Req 27.3). This is the one indication for both "no such doc" and "unreadable
// doc"; the reason is not distinguished to the client.

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
)

// handleGetDoc serves a single platform documentation file from docsDir (Req
// 27.2, 27.3). The wildcard {path} names the file relative to docsDir; the
// handler confines it to that directory and rejects any traversal attempt. By
// default it returns the raw markdown (text/markdown) so the file is byte-for-
// byte the one published to GitHub (Req 27.4); with ?format=html it returns the
// server-rendered HTML for in-app display (Req 27.2). A missing or unreadable
// file is the unavailable indication: 404 with the standard envelope (Req 27.3).
func (s *Server) handleGetDoc(w http.ResponseWriter, r *http.Request) {
	rel := chi.URLParam(r, "*")

	abs, ok := s.resolveDocPath(rel)
	if !ok {
		writeError(w, http.StatusNotFound, codeNotFound, "documentation could not be loaded")
		return
	}

	content, err := os.ReadFile(abs)
	if err != nil {
		// Missing, unreadable, or (if the path resolved to one) a directory:
		// all surface as the single unavailable indication (Req 27.3). No
		// filesystem detail leaks to the client.
		writeError(w, http.StatusNotFound, codeNotFound, "documentation could not be loaded")
		return
	}

	if r.URL.Query().Get("format") == "html" {
		htmlBytes, err := markdownRender(string(content))
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "documentation render failed")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(htmlBytes)
		return
	}

	// Default: serve the raw, unmodified markdown (Req 27.1, 27.4).
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// resolveDocPath confines a caller-supplied relative path to the docs directory
// and returns the absolute path to read. It returns ok=false — never a path
// outside docsDir — for any input that would escape the directory: an empty
// path, an absolute path, a path with a ".." segment, or a path whose resolved
// location falls outside the resolved docs root (which also catches a symlink
// pointing out of the tree).
func (s *Server) resolveDocPath(rel string) (string, bool) {
	if rel == "" {
		return "", false
	}
	// Reject absolute paths outright (e.g. "/etc/passwd").
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return "", false
	}
	// Reject any traversal segment before touching the filesystem. Checking the
	// slash-separated request path is robust regardless of OS separator.
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", false
		}
	}

	// Resolve the docs root to an absolute, symlink-free base. If the root
	// itself cannot be resolved, treat every doc as unavailable.
	rootAbs, err := filepath.Abs(s.docsDir)
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = resolved
	}

	target := filepath.Join(rootAbs, filepath.FromSlash(rel))

	// After joining, re-verify the target is still within the root. filepath.Rel
	// yields a path that starts with ".." exactly when target escapes rootAbs.
	relToRoot, err := filepath.Rel(rootAbs, target)
	if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", false
	}

	// Follow symlinks on the target (if it exists) and confirm the real file is
	// still inside the root, so a symlink inside docs/ cannot point outward.
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		realRel, err := filepath.Rel(rootAbs, resolved)
		if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
			return "", false
		}
		target = resolved
	} else if !errors.Is(err, fs.ErrNotExist) {
		// A resolution error other than "not found" (e.g. a broken symlink or a
		// permission problem) is treated as unavailable rather than served.
		return "", false
	}

	return target, true
}
