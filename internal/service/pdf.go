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

package service

// PDF export (design § "PDF Export — Req 22"; Requirements 22.1, 22.2, 22.3).
//
// The export service renders the target Polygon through the SAME render path
// the app uses for on-screen display — for the requesting user's context — into
// a print-only layout that omits toolbars and menus, then converts that HTML to
// PDF via a headless renderer (Req 22.1). Reusing the app's render + Masker seam
// is the whole point of the design: because the PDF is produced from the exact
// masked HTML a non-reveal user would see, masking is inherited automatically —
// a user without Reveal_Permission for the Polygon's Sphere gets masked
// placeholders in the PDF, never plaintext or ciphertext (Req 22.2). If any step
// fails, no file is produced and an error is returned (Req 22.3).
//
// The render path assembled here mirrors the read path used elsewhere:
//   1. Resolve the Polygon within the caller's OWN tenant (the tenant handle
//      comes from RequestContext, never client input), yielding its stored
//      content and owning Sphere. A Record_ID absent from this tenant is simply
//      not accessible — nothing about other tenants leaks (Req 1.6).
//   2. Authorize via the centralized PolicyEngine: the caller must be able to
//      access the owning Sphere, and the reveal flag handed to the Masker is
//      derived from CanReveal for THAT Sphere (Req 4.3, 4.4, 16.6).
//   3. Resolve Cross-Refraction references for the viewer (LinkResolver), then
//      route the content through the single Masker seam with that reveal flag,
//      then render markdown → HTML (markdown.RenderHTML). This is exactly the
//      render route, so PDF masking parity is structural, not re-implemented.
//   4. Wrap the fragment in a print-only document (no toolbars/menus) and hand
//      it to the injected HTMLToPDF converter.
//
// The HTML→PDF conversion is deliberately behind an interface so the production
// implementation can drive a headless browser (the design names chromedp/rod)
// while tests inject a mock and assert on the HTML that would be converted and
// on failure handling — without requiring a running browser in the build.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/markdown"
	"github.com/influence/influence/internal/recordid"
)

// HTMLToPDF converts a complete HTML document into PDF bytes. It is the single
// seam between the app's own rendered HTML and the concrete headless renderer.
// The production implementation drives a headless browser against this HTML
// (design § Technology Choices — "Headless-render to PDF"); tests inject a mock
// so the PDF export logic — render reuse, masking inheritance, and failure
// handling — is verifiable without a browser dependency.
//
// Convert MUST return a non-nil error and no bytes when it cannot produce a PDF,
// so PDFExporter can honor "on failure, produce no file" (Req 22.3).
type HTMLToPDF interface {
	Convert(ctx context.Context, htmlDoc []byte) ([]byte, error)
}

// ErrPDFRendererUnavailable is returned by the default (unconfigured) renderer
// when no headless renderer has been wired in. It keeps the build and tests free
// of a heavyweight browser dependency: a deployment that wants real PDFs injects
// a concrete HTMLToPDF (e.g. a chromedp/rod-backed one), while environments that
// have not configured one get a clear, non-crashing error rather than a linked
// browser requirement.
var ErrPDFRendererUnavailable = errors.New("pdf: no headless renderer configured")

// ErrPDFGenerationFailed wraps any failure to produce the PDF (render assembly
// or the headless conversion) so the caller can uniformly report an error to the
// user with no file produced (Req 22.3). The underlying cause is wrapped for
// logs but the export always yields zero bytes on failure.
var ErrPDFGenerationFailed = errors.New("pdf: generation failed")

// unconfiguredRenderer is the default HTMLToPDF used when a PDFExporter is built
// without an explicit renderer. It never produces a PDF; it fails closed with
// ErrPDFRendererUnavailable so no partial or bogus file is emitted.
type unconfiguredRenderer struct{}

// Convert always fails: an unconfigured deployment cannot produce a PDF, and
// failing here (rather than importing a browser) keeps "no file on failure"
// (Req 22.3) intact.
func (unconfiguredRenderer) Convert(context.Context, []byte) ([]byte, error) {
	return nil, ErrPDFRendererUnavailable
}

// PDFExporter produces a PDF of a single Polygon by reusing the app's render +
// Masker path for the requesting user, then converting the resulting print-only
// HTML via an injected headless renderer. It holds the tenant-routing gateway,
// the centralized PolicyEngine, the Cross-Refraction resolver, and the HTMLToPDF
// converter. It carries no request state and is safe to share across goroutines.
type PDFExporter struct {
	conns    data.ConnManager
	policy   PolicyEngine
	links    *LinkResolver
	renderer HTMLToPDF
}

// NewPDFExporter constructs a PDFExporter over the connection manager, the
// centralized PolicyEngine, and the Cross-Refraction LinkResolver. The renderer
// is the HTML→PDF converter; a nil renderer falls back to a fail-closed default
// so the exporter never silently emits an empty or invalid file. Production
// wires a headless-browser-backed renderer; tests inject a mock.
func NewPDFExporter(conns data.ConnManager, policy PolicyEngine, links *LinkResolver, renderer HTMLToPDF) *PDFExporter {
	if renderer == nil {
		renderer = unconfiguredRenderer{}
	}
	return &PDFExporter{conns: conns, policy: policy, links: links, renderer: renderer}
}

// PDFExport renders the target Polygon through the app's own render path for the
// requesting user and returns the generated PDF bytes.
//
// It resolves the Polygon within the caller's tenant, authorizes access to the
// owning Sphere, and derives the Masker's reveal flag from CanReveal for that
// Sphere. The stored content is passed through Cross-Refraction resolution, then
// the single Masker seam (so a non-reveal user's HTML — and therefore the PDF —
// contains only placeholders, never plaintext or ciphertext, Req 22.2), then
// rendered to HTML and wrapped in a print-only layout that omits all toolbars
// and menus (Req 22.1). That HTML is converted to PDF by the injected renderer.
//
// On ANY failure — Polygon not accessible, render assembly error, or a renderer
// failure — PDFExport returns nil bytes and an error, so no file is produced
// (Req 22.3). A caller lacking access to the owning Sphere gets
// ErrPolygonNotAccessible (leaking no existence distinction, Req 1.6); a
// conversion failure surfaces as ErrPDFGenerationFailed.
func (e *PDFExporter) PDFExport(ctx context.Context, rc data.RequestContext, authz AuthzData, polygonRecordID string) ([]byte, error) {
	htmlDoc, err := e.renderPrintHTML(ctx, rc, authz, polygonRecordID)
	if err != nil {
		// Render-assembly failures (including "not accessible") return no file.
		return nil, err
	}

	pdf, err := e.renderer.Convert(ctx, htmlDoc)
	if err != nil {
		// Conversion failed: abort the export, produce no file, return an error
		// indication (Req 22.3). The concrete cause is wrapped for logs.
		return nil, fmt.Errorf("%w: %v", ErrPDFGenerationFailed, err)
	}
	if len(pdf) == 0 {
		// A renderer that returns no error but no bytes has not produced a file;
		// treat it as a failure rather than emitting an empty PDF (Req 22.3).
		return nil, ErrPDFGenerationFailed
	}
	return pdf, nil
}

// renderPrintHTML assembles the print-only HTML document for the target Polygon
// in the requesting user's context. It is the render-route reuse: tenant-scoped
// load, Sphere authorization, Cross-Refraction resolution, the single Masker
// seam, markdown→HTML, and a print-only wrapper with no toolbars/menus. It is
// separated from conversion so tests can reason about exactly what HTML would be
// handed to the renderer.
func (e *PDFExporter) renderPrintHTML(ctx context.Context, rc data.RequestContext, authz AuthzData, polygonRecordID string) ([]byte, error) {
	pid, err := recordid.Parse(polygonRecordID)
	if err != nil {
		// An unparseable Record_ID can match no row; treat it as not accessible
		// rather than leaking a parse-vs-missing distinction (Req 1.6).
		return nil, ErrPolygonNotAccessible
	}

	tdb, err := e.conns.Tenant(ctx, rc)
	if err != nil {
		if errors.Is(err, data.ErrTenantNotFound) {
			return nil, ErrPolygonNotAccessible
		}
		return nil, fmt.Errorf("resolve tenant: %w", err)
	}
	db := tdb.DB()

	// Load the Polygon's owning Sphere and stored content, scoped to this
	// tenant's DB. An absent Record_ID is not accessible (Req 1.6).
	var sphereID data.SphereID
	var content string
	err = db.QueryRowContext(ctx,
		`SELECT sphere_id, content FROM polygon WHERE record_id = ?`, pid.Canonical(),
	).Scan(&sphereID, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPolygonNotAccessible
	}
	if err != nil {
		return nil, fmt.Errorf("load polygon content: %w", err)
	}

	// Authorize access to the owning Sphere. No access means the export is not
	// permitted and, like a missing Polygon, reveals no content (Req 4.3, 4.5).
	access, err := e.policy.CanAccessSphere(rc, authz, sphereID)
	if err != nil {
		return nil, fmt.Errorf("authorize sphere: %w", err)
	}
	if access <= data.AccessNone {
		return nil, ErrPolygonNotAccessible
	}

	// Reveal flag for the Masker is derived per-Sphere from the caller's
	// authorization (Req 16.6). A non-reveal user's content is masked below.
	reveal := e.policy.CanReveal(rc, authz, sphereID)

	// Resolve Cross-Refraction references for this viewer before masking, using
	// the same resolver the render route uses (Req 14.2–14.4, 15.3). A nil
	// resolver simply skips link rewriting.
	resolved := content
	if e.links != nil {
		resolved, err = e.links.ResolveContent(ctx, rc, authz, content)
		if err != nil {
			return nil, fmt.Errorf("resolve links: %w", err)
		}
	}

	// Route through the SINGLE Masker seam. With reveal == false every
	// Obfuscation_Token becomes a placeholder and neither plaintext nor
	// ciphertext reaches the HTML — and therefore the PDF (Req 22.2). The
	// per-token revealer is nil here: PDF export shows fully-revealed content
	// only when the whole context is reveal-permitted, mirroring the render
	// route's default view.
	masked := Mask(resolved, reveal, e.revealerFor(ctx, rc, authz))

	// Render the masked markdown to display HTML via the same renderer the app
	// uses on screen (Req 12.1). Reusing markdown.RenderHTML is what guarantees
	// the PDF's body is byte-identical to the rendered view.
	body, err := markdown.RenderHTML([]byte(masked))
	if err != nil {
		return nil, fmt.Errorf("render html: %w", err)
	}

	// Wrap the rendered body in a print-only document that omits every toolbar
	// and menu (Req 22.1). The layout carries only the Polygon content.
	return printLayout(body), nil
}

// revealerFor returns the per-token Revealer used by the Masker when the context
// is reveal-permitted. For the default PDF view we do not perform per-token
// decryption — the reveal flag alone decides masking — so this returns nil,
// which makes Mask treat every token as masked unless the reveal flag is set and
// a revealer is supplied. Keeping it as a seam lets a future caller inject
// per-token reveal for exported content without touching the render assembly.
func (e *PDFExporter) revealerFor(context.Context, data.RequestContext, AuthzData) Revealer {
	return nil
}

// printDocPrefix and printDocSuffix bracket the rendered Polygon body in a
// minimal, self-contained HTML document intended for printing. It deliberately
// contains NO toolbars, menus, navigation, or editor chrome — only the content
// and print-oriented styling (Req 22.1). The @media print / @page rules keep the
// headless renderer's output to the content alone.
const printDocPrefix = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>%s</title>
<style>
@page { margin: 1.5cm; }
body { font-family: Georgia, "Times New Roman", serif; line-height: 1.5; color: #111; }
.polygon-content { max-width: 48rem; margin: 0 auto; }
img { max-width: 100%%; height: auto; }
</style>
</head>
<body>
<main class="polygon-content">
`

const printDocSuffix = `
</main>
</body>
</html>
`

// printLayout wraps rendered Polygon HTML in the print-only document. The title
// is escaped defensively even though it is a fixed literal here, so a future
// dynamic title cannot inject markup.
func printLayout(body []byte) []byte {
	prefix := fmt.Sprintf(printDocPrefix, html.EscapeString("Polygon"))
	out := make([]byte, 0, len(prefix)+len(body)+len(printDocSuffix))
	out = append(out, prefix...)
	out = append(out, body...)
	out = append(out, printDocSuffix...)
	return out
}
