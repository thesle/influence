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

package api

// The Server is the composition root for the HTTP/WebSocket surface (design.md
// — "API Surface"). It holds every service and repository the handlers delegate
// to and builds the chi router that mounts them behind the middleware chain.
//
// Handlers are deliberately thin: they parse the request, read the immutable
// RequestContext the middleware attached, call the relevant service/repo, and
// shape the response. No business rule lives in this package — the service and
// repo layers own validation, isolation, hierarchy integrity, encryption,
// ordering, and concurrency. This package is only wiring.
//
// PACKAGE PLACEMENT. This lives in internal/api rather than internal/server
// because internal/middleware imports internal/server (for PlaintextGuard);
// putting the router — which depends on internal/middleware.Chain — in
// internal/server would form an import cycle (server → middleware → server).
// internal/api sits above both and depends on middleware, service, repo, ws,
// data, and config without any package depending back on it.

import (
	"github.com/influence/influence/internal/config"
	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/markdown"
	"github.com/influence/influence/internal/repo"
	"github.com/influence/influence/internal/service"
	"github.com/influence/influence/internal/ws"
)

// Server holds the fully-wired service/repository graph and the collaborators
// the handlers use. It is constructed once at startup from the connection
// manager and resolved configuration, and is safe for concurrent use: every
// held value is either stateless or internally synchronized.
type Server struct {
	conns data.ConnManager
	cfg   config.Config

	// docsDir is the directory holding the platform documentation markdown
	// files served by GET /api/docs/{path} (Req 27). It defaults to
	// defaultDocsDir; a deployment or test injects an alternate path via
	// WithDocsDir (so tests can point at a temp dir).
	docsDir string

	// Authentication and authorization.
	auth      *service.AuthService
	policy    service.PolicyEngine
	identity  *service.IdentityService
	twoFactor *service.TwoFactorService

	// First-run administrator bootstrap (Req 31). Unauthenticated: it creates
	// the very first admin for a tenant, before any session can exist.
	bootstrap *service.BootstrapService

	// Admin user lifecycle (Req 32.2).
	adminUsers *service.AdminUserService

	// Admin Group + membership management (Req 5.2, 32.3).
	userGroups *service.UserGroupService

	// Content repositories (tenant-scoped via RequestContext).
	spheres   *repo.SphereRepo
	circles   *repo.CircleRepo
	facets    *repo.FacetRepo
	polygons  *repo.PolygonRepo
	images    *repo.ImageRepo
	search    *repo.SearchRepo
	quick     *repo.QuickSearch
	bookmarks *repo.BookmarkRepo
	locks     *repo.EditLockRepo
	index     *repo.PolygonIndex

	// Services.
	encryption *service.EncryptionService
	links      *service.LinkResolver
	pdf        *service.PDFExporter
	footer     *service.MetadataFooterBuilder
	tenants    *service.TenantService

	// Real-time collaboration.
	hub          *ws.Hub
	modeResolver ws.ModeResolver
}

// NewServer wires the whole service/repository graph over a connection manager
// and the resolved configuration. The master secret used to unwrap per-tenant
// key material and the data directory (for tenant export/create) come from cfg.
//
// The PolicyEngine is the single centralized authorization decision-maker every
// component shares; the LinkResolver and PDFExporter both take it so link
// resolution and PDF masking obey the same rules as the middleware guards. The
// PDF renderer is left nil here (the fail-closed default) so the build carries
// no headless-browser dependency; a deployment that wants real PDFs injects one
// via WithPDFRenderer.
func NewServer(conns data.ConnManager, cfg config.Config) *Server {
	policy := service.NewPolicyEngine()

	polygons := repo.NewPolygonRepo(conns)
	links := service.NewLinkResolver(conns, policy)

	// The search index is populated with MASKED content so plaintext/ciphertext
	// never reaches the FTS table (Req 16.7, 23.2). Wire the masking seam with
	// reveal disabled.
	index := repo.NewPolygonIndex(conns, func(content string) string {
		return service.Mask(content, false, nil)
	})

	return &Server{
		conns:     conns,
		cfg:       cfg,
		auth:      service.NewAuthService(conns),
		policy:    policy,
		identity:  service.NewIdentityService(conns),
		twoFactor: service.NewTwoFactorService(conns),

		bootstrap:  service.NewBootstrapService(conns),
		adminUsers: service.NewAdminUserService(conns),
		userGroups: service.NewUserGroupService(conns),

		spheres:   repo.NewSphereRepo(conns),
		circles:   repo.NewCircleRepo(conns),
		facets:    repo.NewFacetRepo(conns),
		polygons:  polygons,
		images:    repo.NewImageRepo(conns),
		search:    repo.NewSearchRepo(conns),
		quick:     repo.NewQuickSearch(conns),
		bookmarks: repo.NewBookmarkRepo(conns),
		locks:     repo.NewEditLockRepo(conns),
		index:     index,

		encryption: service.NewEncryptionService(conns, cfg.MasterSecret),
		links:      links,
		pdf:        service.NewPDFExporter(conns, policy, links, nil),
		footer:     service.NewMetadataFooterBuilder(conns),
		tenants:    service.NewTenantService(conns, cfg.DataDir, cfg.MasterSecret),

		hub:          ws.NewHub(),
		modeResolver: ws.NewRepoModeResolver(polygons),

		docsDir: defaultDocsDir,
	}
}

// defaultDocsDir is the directory the docs handler reads from when nothing is
// injected. It is relative to the process working directory, matching the
// repository's top-level docs/ folder where the platform documentation lives.
const defaultDocsDir = "docs"

// WithDocsDir overrides the directory the documentation handler serves from. It
// returns the Server for chaining so a deployment (or a test pointing at a temp
// dir) can wire an alternate docs location at startup. An empty path is a no-op.
func (s *Server) WithDocsDir(dir string) *Server {
	if dir != "" {
		s.docsDir = dir
	}
	return s
}

// WithPDFRenderer replaces the fail-closed default PDF renderer with a concrete
// headless-browser-backed converter. It returns the Server for chaining so a
// deployment can wire a real renderer at startup. Passing nil is a no-op.
func (s *Server) WithPDFRenderer(renderer service.HTMLToPDF) *Server {
	if renderer != nil {
		s.pdf = service.NewPDFExporter(s.conns, s.policy, s.links, renderer)
	}
	return s
}

// markdownRender is the shared markdown → HTML render used by the render route.
// It is a thin adapter over the markdown package kept here so the render handler
// stays a one-liner delegation.
func markdownRender(content string) ([]byte, error) {
	return markdown.RenderHTML([]byte(content))
}
