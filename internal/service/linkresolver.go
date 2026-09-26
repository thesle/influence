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

// Cross-Refraction link resolution (design § Link rendering — "Cross-Refraction
// (Req 14)"; Requirements 14.1–14.4, 15.1, 15.3).
//
// Internal / Cross-Refraction links are STORED by the target Polygon's
// Record_ID, never by name: the stored markdown holds
// `[label](influence://polygon/{record_id})` (design § Storage model, Req 14.1,
// 15.1). Because the reference is the immutable Record_ID (Req 15.5), renaming
// the target changes nothing about the stored link — the same reference simply
// resolves to the target's *current* name the next time the page renders
// (Req 15.3). Resolution is therefore a render-time concern and lives here on
// the read path rather than being baked into stored content.
//
// At render each reference is resolved to exactly one of three outcomes, keyed
// on the target's state relative to the viewer:
//
//   - ACTIVATABLE (Req 14.2, 15.3): the target Polygon exists in the viewer's
//     tenant AND the viewer can access its owning Sphere. The link is
//     activatable and its visible text is the target's CURRENT name (derived
//     fresh from the target's content on every resolve, so a rename is reflected
//     immediately — Req 15.3).
//   - UNAVAILABLE (Req 14.3): the target Record_ID cannot be resolved because
//     the Polygon has been deleted (or never existed / is unparseable). The link
//     is rendered non-activatable and marked "unavailable".
//   - INACCESSIBLE (Req 14.4): the target exists but lives in a Sphere the viewer
//     cannot access. The link is rendered non-activatable, marked "inaccessible",
//     and the target's name is WITHHELD — the resolver never emits the name of a
//     Polygon in a Sphere the viewer cannot reach.
//
// Like the encryption/reveal services, LinkResolver reaches the database only
// through the caller's own tenant handle (ConnManager.Tenant scoped to the
// RequestContext), so a Record_ID that belongs to another tenant is simply
// absent here and resolves to "unavailable" — nothing about other tenants can
// leak (Req 1.5, 1.6). The per-Sphere accessibility decision is delegated to the
// same centralized PolicyEngine every other access check uses (Req 4.3, 4.5,
// 4.7, 5.4), so link resolution can never be more permissive than the platform's
// authorization rules.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/markdown"
	"github.com/influence/influence/internal/recordid"
)

// linkScheme is the internal-link URL scheme prefix. Every Cross-Refraction /
// internal link stored in content is `influence://polygon/{record_id}` (design
// § Storage model, Req 14.1, 15.1).
const linkScheme = "influence://polygon/"

// crossRefractionRe matches a single stored Cross-Refraction / internal link in
// markdown link form: `[label](influence://polygon/{record_id})`. The label and
// the record_id are captured. The label is intentionally permissive (any run of
// non-`]` characters) because it is the author's original text and is discarded
// at resolve time in favor of the target's current name; the record_id captures
// everything up to the closing paren and is validated by recordid.Parse.
var crossRefractionRe = regexp.MustCompile(`\[([^\]]*)\]\(` + regexp.QuoteMeta(linkScheme) + `([^)]+)\)`)

// LinkState is the resolved rendering state of one Cross-Refraction reference.
type LinkState int

const (
	// LinkActivatable means the target exists and the viewer can access its
	// Sphere; the link is activatable and shows the target's current name
	// (Req 14.2, 15.3).
	LinkActivatable LinkState = iota
	// LinkUnavailable means the target Record_ID could not be resolved because
	// the Polygon has been deleted (or never existed); the link is
	// non-activatable and marked unavailable (Req 14.3).
	LinkUnavailable
	// LinkInaccessible means the target exists but resides in a Sphere the
	// viewer cannot access; the link is non-activatable, the name is withheld,
	// and it is marked inaccessible (Req 14.4).
	LinkInaccessible
)

// String renders a LinkState as a short, stable token, handy in tests and logs.
func (s LinkState) String() string {
	switch s {
	case LinkActivatable:
		return "activatable"
	case LinkUnavailable:
		return "unavailable"
	case LinkInaccessible:
		return "inaccessible"
	default:
		return "unknown"
	}
}

// ResolvedLink is the outcome of resolving one Cross-Refraction reference.
// TargetRecordID is the referenced Polygon's canonical Record_ID (echoed back
// even for unavailable/inaccessible targets, since it is the stored reference
// and reveals nothing about content). Name is the target's current name and is
// populated ONLY for an activatable link; for unavailable and inaccessible
// links it is empty — inaccessible deliberately withholds the name (Req 14.4).
// Activatable reports whether the link may be followed.
type ResolvedLink struct {
	// TargetRecordID is the canonical Record_ID the link references (Req 15.1).
	TargetRecordID string
	// State is the resolved state: activatable, unavailable, or inaccessible.
	State LinkState
	// Name is the target Polygon's current name, set only when State is
	// LinkActivatable (Req 14.2, 15.3); empty otherwise.
	Name string
	// Activatable is true exactly when State is LinkActivatable.
	Activatable bool
}

// unavailableLabel / inaccessibleLabel are the visible texts a non-activatable
// link carries so a reader sees why it cannot be followed (Req 14.3, 14.4). They
// deliberately contain no target name.
const (
	unavailableLabel  = "unavailable"
	inaccessibleLabel = "inaccessible"
)

// LinkResolver resolves Cross-Refraction references at render time. It holds the
// tenant-routing gateway and the PolicyEngine; every accessibility decision is
// delegated to the engine so link resolution obeys the same authorization rules
// as the rest of the platform. It carries no request state and is safe to share
// across goroutines.
type LinkResolver struct {
	conns  data.ConnManager
	policy PolicyEngine
}

// NewLinkResolver constructs a LinkResolver over the connection manager and the
// centralized PolicyEngine.
func NewLinkResolver(conns data.ConnManager, policy PolicyEngine) *LinkResolver {
	return &LinkResolver{conns: conns, policy: policy}
}

// ResolveContent rewrites every Cross-Refraction reference in content to its
// resolved render form for the given viewer, returning the rewritten markdown.
//
// Each `[label](influence://polygon/{id})` reference is resolved independently
// against the viewer's own tenant and authorization (authz):
//
//   - target exists and is accessible → an activatable link whose visible text
//     is the target's CURRENT name, i.e. `[name](influence://polygon/{id})`
//     (Req 14.2, 15.3). Because the name is re-derived on every call, a rename
//     is reflected immediately while the stored reference (the Record_ID) is
//     unchanged (Req 15.1, 15.3).
//   - target deleted / unresolvable → a non-activatable marker
//     `[label — unavailable]` that carries no URL (Req 14.3).
//   - target in an inaccessible Sphere → a non-activatable marker
//     `[inaccessible]` that carries no URL and no target name (Req 14.4).
//
// Non-link content is passed through untouched. Resolution is read-only; it
// never mutates stored content.
func (r *LinkResolver) ResolveContent(ctx context.Context, rc data.RequestContext, authz AuthzData, content string) (string, error) {
	// Fast path: nothing to resolve if there is no internal-link scheme present.
	if !strings.Contains(content, linkScheme) {
		return content, nil
	}

	// Resolve the tenant handle once and reuse it across every reference in the
	// document. A tenant that cannot be reached means none of its Polygons can
	// be reached either, so every reference resolves to "unavailable" — the same
	// outcome as a deleted target, leaking nothing (Req 1.6, 14.3).
	tdb, err := r.conns.Tenant(ctx, rc)
	if err != nil {
		if errors.Is(err, data.ErrTenantNotFound) {
			return r.rewriteAll(ctx, rc, authz, content, nil), nil
		}
		return "", fmt.Errorf("resolve tenant: %w", err)
	}

	return r.rewriteAll(ctx, rc, authz, content, tdb.DB()), nil
}

// rewriteAll replaces every Cross-Refraction reference in content with its
// resolved form. db may be nil, in which case every reference resolves to
// "unavailable" (no reachable tenant). A per-call cache keyed by canonical
// Record_ID avoids re-querying the same target twice within one document.
func (r *LinkResolver) rewriteAll(ctx context.Context, rc data.RequestContext, authz AuthzData, content string, db *sql.DB) string {
	cache := make(map[string]ResolvedLink)

	return crossRefractionRe.ReplaceAllStringFunc(content, func(match string) string {
		groups := crossRefractionRe.FindStringSubmatch(match)
		// groups[1] = original label (discarded), groups[2] = raw record id.
		label := groups[1]
		rawID := groups[2]

		resolved, ok := cache[rawID]
		if !ok {
			resolved = r.resolveOne(ctx, rc, authz, db, rawID)
			cache[rawID] = resolved
		}
		return renderResolved(label, rawID, resolved)
	})
}

// resolveOne resolves a single reference by raw record id against the viewer's
// tenant and authorization. It never returns an error: any resolution failure
// collapses into "unavailable", which is the correct render outcome for a target
// that cannot be reached (Req 14.3) and avoids leaking a failure distinction.
func (r *LinkResolver) resolveOne(ctx context.Context, rc data.RequestContext, authz AuthzData, db *sql.DB, rawID string) ResolvedLink {
	id, err := recordid.Parse(rawID)
	if err != nil {
		// A malformed reference can match no Polygon; render it as unavailable
		// rather than surfacing a parse error into the page.
		return ResolvedLink{TargetRecordID: rawID, State: LinkUnavailable}
	}
	canonical := id.Canonical()

	if db == nil {
		return ResolvedLink{TargetRecordID: canonical, State: LinkUnavailable}
	}

	// Look up the target's owning Sphere and current content in one query,
	// scoped to the viewer's own tenant DB. A Record_ID absent from this tenant
	// (deleted, never existed, or belonging to another tenant) yields no row and
	// is "unavailable" (Req 14.3, 1.6).
	var sphereID data.SphereID
	var targetContent string
	err = db.QueryRowContext(ctx,
		`SELECT sphere_id, content FROM polygon WHERE record_id = ?`, canonical,
	).Scan(&sphereID, &targetContent)
	if errors.Is(err, sql.ErrNoRows) {
		return ResolvedLink{TargetRecordID: canonical, State: LinkUnavailable}
	}
	if err != nil {
		// A query error means we cannot confirm the target exists; fail closed
		// to "unavailable" rather than emitting a possibly-wrong activatable link.
		return ResolvedLink{TargetRecordID: canonical, State: LinkUnavailable}
	}

	// The target exists. Decide accessibility via the centralized PolicyEngine:
	// any access above AccessNone permits viewing the target and therefore an
	// activatable link. A Sphere the viewer cannot reach yields "inaccessible"
	// with the name withheld (Req 14.4).
	access, err := r.policy.CanAccessSphere(rc, authz, sphereID)
	if err != nil || access <= data.AccessNone {
		return ResolvedLink{TargetRecordID: canonical, State: LinkInaccessible}
	}

	// Accessible: resolve to the target's CURRENT name (Req 14.2, 15.3).
	return ResolvedLink{
		TargetRecordID: canonical,
		State:          LinkActivatable,
		Name:           PolygonName(targetContent),
		Activatable:    true,
	}
}

// Resolve resolves a single reference by its raw Record_ID for the given viewer,
// returning the structured outcome. It is the programmatic counterpart to
// ResolveContent for callers (e.g. an API endpoint or the frontend) that hold a
// bare Record_ID rather than a blob of content. The three states and the
// name-withholding rule are identical to ResolveContent (Req 14.2–14.4, 15.3).
func (r *LinkResolver) Resolve(ctx context.Context, rc data.RequestContext, authz AuthzData, targetRecordID string) (ResolvedLink, error) {
	tdb, err := r.conns.Tenant(ctx, rc)
	if err != nil {
		if errors.Is(err, data.ErrTenantNotFound) {
			return r.resolveOne(ctx, rc, authz, nil, targetRecordID), nil
		}
		return ResolvedLink{}, fmt.Errorf("resolve tenant: %w", err)
	}
	return r.resolveOne(ctx, rc, authz, tdb.DB(), targetRecordID), nil
}

// renderResolved renders one resolved reference back to markdown. An activatable
// link keeps the internal-link URL and swaps the visible text to the target's
// current name (Req 14.2, 15.3). A non-activatable link carries NO URL so it
// cannot be followed: "unavailable" keeps the author's original label for
// context (Req 14.3) while "inaccessible" shows only the marker, withholding the
// target name entirely (Req 14.4).
func renderResolved(originalLabel, rawID string, resolved ResolvedLink) string {
	switch resolved.State {
	case LinkActivatable:
		return fmt.Sprintf("[%s](%s%s)", resolved.Name, linkScheme, resolved.TargetRecordID)
	case LinkInaccessible:
		// Withhold the name: no target name, no URL (Req 14.4).
		return fmt.Sprintf("[%s]", inaccessibleLabel)
	default: // LinkUnavailable
		// Keep the author's label for context but drop the URL so it is not
		// activatable, and mark it unavailable (Req 14.3).
		if strings.TrimSpace(originalLabel) == "" {
			return fmt.Sprintf("[%s]", unavailableLabel)
		}
		return fmt.Sprintf("[%s — %s]", originalLabel, unavailableLabel)
	}
}

// PolygonName derives a Polygon's current display name from its stored content.
// A Markdown_Page has no separate name column; its name is its title — the text
// of its first heading (design § Storage model / @-command headings). Deriving
// the name from content on every resolve is what makes "rename" work for
// Cross-Refraction: editing the first heading changes the resolved name while
// the Record_ID (and therefore every stored link) is untouched (Req 15.1, 15.3).
//
// When the content has no heading, PolygonName falls back to the first
// non-empty line of text, and finally to "Untitled" for empty content, so an
// accessible target always resolves to a non-empty, activatable name (Req 14.2).
func PolygonName(content string) string {
	for _, s := range markdown.Sections([]byte(content)) {
		if s.Level >= 1 {
			if h := strings.TrimSpace(s.Heading); h != "" {
				return h
			}
		}
	}
	// No usable heading: fall back to the first non-empty line of text.
	for _, line := range strings.Split(content, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return "Untitled"
}
