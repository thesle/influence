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

package ws

import (
	"context"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/repo"
)

// RepoModeResolver adapts the tenant-scoped Polygon repository to the hub's
// ModeResolver interface. It is the production wiring that lets the hub read a
// Polygon's edit_mode (Req 25.1) straight from the tenant DB while the hub
// itself stays decoupled from the repository.
type RepoModeResolver struct {
	repo *repo.PolygonRepo
}

// NewRepoModeResolver constructs a RepoModeResolver over a PolygonRepo.
func NewRepoModeResolver(r *repo.PolygonRepo) *RepoModeResolver {
	return &RepoModeResolver{repo: r}
}

// Mode resolves the Polygon's stored edit mode within rc's tenant and maps it
// onto the hub's Mode type (Req 25.1). A Polygon in another tenant or absent
// entirely surfaces the repository's ErrNotAccessible unchanged (Req 1.6). A
// stored value outside the supported set is passed through as an invalid Mode;
// callers may check Mode.Valid.
func (a *RepoModeResolver) Mode(ctx context.Context, rc data.RequestContext, polygonRecordID string) (Mode, error) {
	em, err := a.repo.Mode(ctx, rc, polygonRecordID)
	if err != nil {
		return "", err
	}
	return Mode(em), nil
}
