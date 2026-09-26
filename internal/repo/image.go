package repo

// This file implements image paste storage, retrieval, and reference resolution
// (design.md — "Image Blob Handling — Req 17", Requirements 17.1, 17.2, 17.3,
// 17.4, 17.5). Pasted images live entirely inside the caller's tenant DB as
// rows in IMAGE_BLOB, so an exported tenant file carries its images with it and
// there is no external content store (Req 2.1, 17.1).
//
// Three behaviors live here:
//
//   - StoreImage (Req 17.1, 17.2, 17.3). Validation is done on the ACTUAL
//     content type sniffed from the bytes, never a client-declared MIME: the
//     bytes must decode as one of PNG/JPEG/GIF/WebP and be no larger than
//     10 MB. A failing paste stores nothing (the IMAGE_BLOB table is untouched)
//     and returns a specific sentinel — ErrUnsupportedImageFormat for a bad
//     format, ErrImageTooLarge for oversize — so the editor can leave the
//     Polygon content unchanged and show the right message. On success the
//     bytes are stored under a fresh image_id and the caller receives the
//     `influence://image/{image_id}` reference to insert into the content.
//
//   - GetImage (Req 17.4). Returns the stored bytes and MIME for a given
//     image_id from the caller's tenant so the render/serving path can stream
//     the blob. An image_id absent from the caller's tenant is ErrNotAccessible
//     — indistinguishable from one owned by another tenant (Req 1.6).
//
//   - ResolveImageReference (Req 17.4, 17.5). Maps an `influence://image/{id}`
//     reference found in Polygon content to the URL the renderer should emit for
//     the served blob when the image exists in the caller's tenant, or to a
//     placeholder when the reference is malformed or unresolvable.
//
// Everything is tenant-scoped through Base.tenant, so one tenant's images are
// never reachable from another's context (Req 1.4, 1.5, 1.6).

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// MaxImageBytes is the inclusive upper bound on a pasted image's size: 10 MB
// (Req 17.3). A paste whose byte length exceeds this is rejected with
// ErrImageTooLarge and stored nowhere.
const MaxImageBytes = 10 * 1024 * 1024

// imageRefScheme is the fixed prefix of an in-content image reference. A stored
// image is referenced as imageRefScheme + image_id, e.g.
// "influence://image/{image_id}" (Req 17.1).
const imageRefScheme = "influence://image/"

// ErrUnsupportedImageFormat is returned when pasted bytes do not sniff as one of
// the supported image formats (PNG, JPEG, GIF, WebP). It is deliberately
// distinct from ErrImageTooLarge so the editor can show the format-specific
// message and leave the Polygon content unchanged (Req 17.2).
var ErrUnsupportedImageFormat = errors.New("unsupported image format")

// ErrImageTooLarge is returned when pasted bytes exceed MaxImageBytes. It is
// distinct from ErrUnsupportedImageFormat so the editor can show the size
// message and leave the Polygon content unchanged (Req 17.3).
var ErrImageTooLarge = errors.New("image exceeds maximum allowed size")

// supportedImageMIME is the closed allow-list of MIME types a pasted image may
// resolve to (Req 17.2). A sniffed type outside this set is unsupported.
var supportedImageMIME = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// ImageRepo is the tenant-scoped repository for pasted image blobs. It embeds
// Base so it shares the single tenant-routing gateway; it holds no state of its
// own.
type ImageRepo struct {
	Base
}

// NewImageRepo constructs an ImageRepo over a ConnManager.
func NewImageRepo(conns data.ConnManager) *ImageRepo {
	return &ImageRepo{Base: NewBase(conns)}
}

// Image is a stored image blob: its serving MIME type and raw bytes. Returned by
// GetImage for the render/serving path (Req 17.4).
type Image struct {
	MIME string
	Data []byte
}

// StoreImage validates and stores a pasted image in the caller's tenant,
// returning the `influence://image/{image_id}` reference to insert into the
// Polygon content on success (Req 17.1).
//
// Validation runs against the ACTUAL content type sniffed from the bytes, not
// any client-declared MIME (which is not trusted): the bytes must sniff as
// PNG/JPEG/GIF/WebP (Req 17.2) and be at most MaxImageBytes (Req 17.3). A
// failing paste returns ErrUnsupportedImageFormat or ErrImageTooLarge and
// stores nothing — the IMAGE_BLOB table is left unchanged, so the editor can
// leave the Polygon content unchanged (Req 17.2, 17.3).
//
// The size check is applied before the format sniff so an oversize paste is
// reported as too-large regardless of its bytes.
func (r *ImageRepo) StoreImage(ctx context.Context, rc data.RequestContext, data_ []byte) (string, error) {
	// Size first (Req 17.3): an oversize paste is rejected as too-large no
	// matter what its bytes decode to. Nothing is stored.
	if len(data_) > MaxImageBytes {
		return "", fmt.Errorf("%w: %d bytes exceeds %d", ErrImageTooLarge, len(data_), MaxImageBytes)
	}

	// Format next (Req 17.2): the sniffed type must be in the allow-list.
	// Nothing is stored on a bad format.
	mime, ok := sniffImageMIME(data_)
	if !ok {
		return "", fmt.Errorf("%w", ErrUnsupportedImageFormat)
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return "", err
	}

	// A fresh, immutable image_id (Req 15.5 semantics). Stored canonically.
	imageID := recordid.New()

	if _, err := tdb.DB().ExecContext(ctx,
		`INSERT INTO image_blob (image_id, data, mime) VALUES (?, ?, ?)`,
		imageID.Canonical(), data_, mime,
	); err != nil {
		return "", fmt.Errorf("store image: %w", err)
	}

	return imageRefScheme + imageID.Canonical(), nil
}

// GetImage returns the stored bytes and MIME for image_id from the caller's
// tenant so the serving path can stream the blob (Req 17.4). An image_id absent
// from the caller's tenant — nonexistent or owned by another tenant — yields
// ErrNotAccessible, indistinguishable by design (Req 1.6).
func (r *ImageRepo) GetImage(ctx context.Context, rc data.RequestContext, imageID string) (Image, error) {
	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return Image{}, err
	}

	var img Image
	err = tdb.DB().QueryRowContext(ctx,
		`SELECT mime, data FROM image_blob WHERE image_id = ?`, imageID,
	).Scan(&img.MIME, &img.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return Image{}, ErrNotAccessible
	}
	if err != nil {
		return Image{}, fmt.Errorf("get image: %w", err)
	}
	return img, nil
}

// ImagePlaceholderURL is the value the renderer emits for an image reference it
// cannot resolve, signaling that the image is unavailable (Req 17.5). Keeping
// it a single well-known constant lets the render/view layer render a
// consistent "unavailable" placeholder.
const ImagePlaceholderURL = "influence://image-unavailable"

// ResolveImageReference maps an in-content image reference to what the renderer
// should emit. When ref is a well-formed `influence://image/{image_id}` whose
// image exists in the caller's tenant, it returns the served-blob URL and
// resolved=true (Req 17.4). When ref is malformed or its image_id is
// unresolvable within the tenant, it returns ImagePlaceholderURL and
// resolved=false so the renderer shows an "unavailable" placeholder (Req 17.5).
//
// servedURLFor builds the served-blob URL from a resolved image_id; the render
// layer supplies it (typically the images GET route). It must be non-nil.
func (r *ImageRepo) ResolveImageReference(ctx context.Context, rc data.RequestContext, ref string, servedURLFor func(imageID string) string) (string, bool) {
	imageID, ok := ParseImageReference(ref)
	if !ok {
		// Not an image reference we own — placeholder (Req 17.5).
		return ImagePlaceholderURL, false
	}

	// The reference resolves only if the blob exists in the caller's tenant.
	if _, err := r.GetImage(ctx, rc, imageID); err != nil {
		// Absent (nonexistent or another tenant's) or any lookup error — the
		// reference is unresolvable, so render a placeholder (Req 17.5).
		return ImagePlaceholderURL, false
	}

	return servedURLFor(imageID), true
}

// ParseImageReference extracts the image_id from an
// `influence://image/{image_id}` reference. It returns ok=false when ref does
// not use the image scheme or carries an empty id, so callers can treat a
// malformed reference as unresolvable (Req 17.5).
func ParseImageReference(ref string) (string, bool) {
	if len(ref) <= len(imageRefScheme) || ref[:len(imageRefScheme)] != imageRefScheme {
		return "", false
	}
	id := ref[len(imageRefScheme):]
	if id == "" {
		return "", false
	}
	return id, true
}

// sniffImageMIME determines the ACTUAL image type of data by inspecting its
// bytes (Req 17.2) — the client-declared MIME is never trusted. It returns the
// canonical MIME and ok=true only for a supported type (PNG/JPEG/GIF/WebP).
//
// net/http.DetectContentType recognizes PNG, JPEG, and GIF from their magic
// numbers but does not classify WebP (it falls back to a generic type), so WebP
// is detected directly from its RIFF/WEBP container signature.
func sniffImageMIME(data []byte) (string, bool) {
	if isWebP(data) {
		return "image/webp", true
	}
	mime := http.DetectContentType(data)
	// DetectContentType may append parameters (e.g. "; charset=..."); the image
	// types it returns do not, but normalize defensively by taking the media
	// type before any ';'.
	if i := bytes.IndexByte([]byte(mime), ';'); i >= 0 {
		mime = mime[:i]
	}
	if supportedImageMIME[mime] {
		return mime, true
	}
	return "", false
}

// isWebP reports whether data begins with the WebP container signature: the
// 4-byte "RIFF" chunk tag, a 4-byte file size, then the "WEBP" form type
// (RIFF/WEBP). This is the same signature libraries use to identify WebP.
func isWebP(data []byte) bool {
	return len(data) >= 12 &&
		bytes.Equal(data[0:4], []byte("RIFF")) &&
		bytes.Equal(data[8:12], []byte("WEBP"))
}
