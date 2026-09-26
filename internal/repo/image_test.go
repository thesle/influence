package repo

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// These tests exercise image paste storage, retrieval, and reference resolution
// over the real production tenant-routing path (the twoTenantFixture from
// base_test.go): file-backed tenants behind a real ConnManager. They verify
// content-type sniffing and validation (Req 17.1, 17.2, 17.3), tenant-scoped
// retrieval (Req 17.4, 1.6), and reference resolution with a placeholder for
// the unresolvable case (Req 17.4, 17.5).

// Minimal valid magic-number headers for each supported format. DetectContentType
// classifies PNG/JPEG/GIF from these; WebP is recognized from its RIFF/WEBP
// container. Padding keeps sniffers happy without embedding real image payloads.
var (
	pngBytes  = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, make([]byte, 16)...)
	jpegBytes = append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}, make([]byte, 16)...)
	gifBytes  = append([]byte("GIF89a"), make([]byte, 16)...)
	webpBytes = func() []byte {
		b := make([]byte, 0, 32)
		b = append(b, "RIFF"...)
		b = append(b, 0x00, 0x00, 0x00, 0x00) // file size (ignored by sniff)
		b = append(b, "WEBP"...)
		b = append(b, make([]byte, 16)...)
		return b
	}()
)

func countImages(t *testing.T, f twoTenantFixture) int {
	t.Helper()
	return countRows(t, f.dbA, "image_blob")
}

// TestStoreImageAcceptsSupportedFormats confirms each supported format is
// sniffed by its actual bytes, stored as a blob, and returns an
// influence://image/{id} reference (Req 17.1).
func TestStoreImageAcceptsSupportedFormats(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		mime string
	}{
		{"png", pngBytes, "image/png"},
		{"jpeg", jpegBytes, "image/jpeg"},
		{"gif", gifBytes, "image/gif"},
		{"webp", webpBytes, "image/webp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTwoTenantFixture(t)
			repo := &ImageRepo{Base: f.base}
			ctx := context.Background()

			ref, err := repo.StoreImage(ctx, f.rcA, tc.data)
			if err != nil {
				t.Fatalf("StoreImage(%s): %v", tc.name, err)
			}
			if !strings.HasPrefix(ref, "influence://image/") {
				t.Errorf("reference = %q, want influence://image/ prefix", ref)
			}

			id, ok := ParseImageReference(ref)
			if !ok {
				t.Fatalf("ParseImageReference(%q) failed", ref)
			}
			img, err := repo.GetImage(ctx, f.rcA, id)
			if err != nil {
				t.Fatalf("GetImage: %v", err)
			}
			if img.MIME != tc.mime {
				t.Errorf("stored mime = %q, want %q", img.MIME, tc.mime)
			}
			if !bytes.Equal(img.Data, tc.data) {
				t.Errorf("stored data does not round-trip")
			}
		})
	}
}

// TestStoreImageRejectsUnsupportedFormat confirms bytes that do not sniff as a
// supported image are rejected with ErrUnsupportedImageFormat and stored nowhere
// (Req 17.2).
func TestStoreImageRejectsUnsupportedFormat(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &ImageRepo{Base: f.base}
	ctx := context.Background()

	// Plain text — not an image of any supported type.
	_, err := repo.StoreImage(ctx, f.rcA, []byte("this is not an image at all, just text"))
	if !errors.Is(err, ErrUnsupportedImageFormat) {
		t.Fatalf("got %v, want ErrUnsupportedImageFormat", err)
	}
	if n := countImages(t, f); n != 0 {
		t.Errorf("image_blob count = %d, want 0 (paste must store nothing)", n)
	}
}

// TestStoreImageRejectsOversize confirms a paste larger than MaxImageBytes is
// rejected with ErrImageTooLarge — distinct from the unsupported-format error —
// and stored nowhere (Req 17.3).
func TestStoreImageRejectsOversize(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &ImageRepo{Base: f.base}
	ctx := context.Background()

	// A valid PNG header padded past the 10 MB limit.
	oversize := append(append([]byte{}, pngBytes...), make([]byte, MaxImageBytes)...)
	_, err := repo.StoreImage(ctx, f.rcA, oversize)
	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("got %v, want ErrImageTooLarge", err)
	}
	if errors.Is(err, ErrUnsupportedImageFormat) {
		t.Errorf("oversize error must be distinct from unsupported-format")
	}
	if n := countImages(t, f); n != 0 {
		t.Errorf("image_blob count = %d, want 0 (paste must store nothing)", n)
	}
}

// TestStoreImageAtSizeLimitAccepted confirms the 10 MB bound is inclusive: a
// paste exactly at MaxImageBytes is accepted (Req 17.3).
func TestStoreImageAtSizeLimitAccepted(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &ImageRepo{Base: f.base}
	ctx := context.Background()

	atLimit := make([]byte, MaxImageBytes)
	copy(atLimit, pngBytes)
	if _, err := repo.StoreImage(ctx, f.rcA, atLimit); err != nil {
		t.Fatalf("StoreImage at limit: %v", err)
	}
}

// TestGetImageCrossTenantNotAccessible confirms an image stored in one tenant is
// not reachable from another tenant's context (Req 1.6, 17.4).
func TestGetImageCrossTenantNotAccessible(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &ImageRepo{Base: f.base}
	ctx := context.Background()

	ref, err := repo.StoreImage(ctx, f.rcA, pngBytes)
	if err != nil {
		t.Fatalf("StoreImage: %v", err)
	}
	id, _ := ParseImageReference(ref)

	// Tenant B cannot see tenant A's image.
	if _, err := repo.GetImage(ctx, f.rcB, id); !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant GetImage: got %v, want ErrNotAccessible", err)
	}
	// The owning tenant can.
	if _, err := repo.GetImage(ctx, f.rcA, id); err != nil {
		t.Fatalf("owning tenant GetImage: %v", err)
	}
}

// TestResolveImageReferenceResolvesStored confirms a reference to a stored image
// resolves to the served URL built by the supplied callback (Req 17.4).
func TestResolveImageReferenceResolvesStored(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &ImageRepo{Base: f.base}
	ctx := context.Background()

	ref, err := repo.StoreImage(ctx, f.rcA, pngBytes)
	if err != nil {
		t.Fatalf("StoreImage: %v", err)
	}

	served := func(id string) string { return "/images/" + id }
	url, ok := repo.ResolveImageReference(ctx, f.rcA, ref, served)
	if !ok {
		t.Fatalf("expected resolved reference, got placeholder %q", url)
	}
	id, _ := ParseImageReference(ref)
	if url != served(id) {
		t.Errorf("resolved url = %q, want %q", url, served(id))
	}
}

// TestResolveImageReferenceUnresolvablePlaceholder confirms an unknown or
// malformed reference resolves to the unavailable placeholder (Req 17.5).
func TestResolveImageReferenceUnresolvablePlaceholder(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &ImageRepo{Base: f.base}
	ctx := context.Background()

	served := func(id string) string { return "/images/" + id }

	cases := []struct {
		name string
		ref  string
	}{
		{"unknown id", "influence://image/00000000-0000-0000-0000-000000000000"},
		{"malformed scheme", "http://example.com/x.png"},
		{"empty id", "influence://image/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, ok := repo.ResolveImageReference(ctx, f.rcA, tc.ref, served)
			if ok {
				t.Errorf("expected placeholder, got resolved %q", url)
			}
			if url != ImagePlaceholderURL {
				t.Errorf("placeholder url = %q, want %q", url, ImagePlaceholderURL)
			}
		})
	}
}
