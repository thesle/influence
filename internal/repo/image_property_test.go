package repo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Feature: influence, Property 22: Image paste validation
//
// For all pasted image byte blobs with a declared type, the image is stored as
// a blob and referenced in content if and only if its sniffed format is PNG,
// JPEG, GIF, or WebP and its size does not exceed 10 MB; an invalid paste is
// rejected and leaves the Polygon content unchanged.
//
// Validates: Requirements 17.1, 17.2, 17.3
//
// The property is exercised over the real production tenant-routing path (the
// twoTenantFixture from base_test.go): a file-backed tenant behind a real
// ConnManager. Each rapid run draws a pasted blob whose two axes — format
// validity and size — are varied independently, then applies StoreImage and
// checks the accept/reject decision against the ground-truth predicate
//
//	accept  <=>  (bytes sniff as a supported image)  AND  (len <= MaxImageBytes)
//
// asserting on success that exactly one blob was stored and an
// influence://image/{id} reference was returned, and on rejection that nothing
// was stored (the blob table is unchanged, so the Polygon content the editor
// would leave untouched) and the correct distinct sentinel was returned
// (ErrImageTooLarge for oversize, ErrUnsupportedImageFormat otherwise).
//
// Generation strategy. Allocating a full 10 MB buffer every iteration would
// make 100+ runs needlessly slow, so sizes are drawn from a modest band around
// a small synthetic "limit" is avoided; instead we vary the *payload* size in a
// small range and separately, with low probability, force the true 10 MB+1
// oversize case so the real boundary is still exercised without paying the
// allocation on every run. The exact MaxImageBytes boundary (inclusive accept)
// and MaxImageBytes+1 (reject) are also asserted explicitly outside the loop so
// the frontier is covered deterministically regardless of sampling.
func TestImagePropertyPasteValidation(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &ImageRepo{Base: f.base}
	ctx := context.Background()

	// Valid magic-number headers for each supported format, reused from
	// image_test.go (pngBytes/jpegBytes/gifBytes/webpBytes). A blob built from
	// one of these headers sniffs as a supported image.
	supportedHeaders := [][]byte{pngBytes, jpegBytes, gifBytes, webpBytes}

	// Byte sequences that do NOT sniff as any supported image: plain text, an
	// empty blob, and a truncated/garbage header. These must be rejected as
	// unsupported.
	unsupportedHeaders := [][]byte{
		[]byte("this is not an image at all, just some text"),
		[]byte{},
		{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07},
		[]byte("RIFF____NOTW"), // RIFF container but not a WEBP form type
	}

	rapid.Check(t, func(rt *rapid.T) {
		// Axis 1: is the header a supported image format?
		validFormat := rapid.Bool().Draw(rt, "validFormat")

		var header []byte
		if validFormat {
			header = supportedHeaders[rapid.IntRange(0, len(supportedHeaders)-1).Draw(rt, "supportedHeader")]
		} else {
			header = unsupportedHeaders[rapid.IntRange(0, len(unsupportedHeaders)-1).Draw(rt, "unsupportedHeader")]
		}

		// Axis 2: size. Most runs stay small (fast); with low probability force
		// a genuine oversize blob so the >10 MB path is sampled during the loop
		// without allocating 10 MB every iteration.
		forceOversize := rapid.Float64Range(0, 1).Draw(rt, "oversizeDraw") < 0.05

		var blob []byte
		if forceOversize {
			// header (if any) followed by enough padding to exceed the limit.
			blob = make([]byte, MaxImageBytes+1)
			copy(blob, header)
		} else {
			pad := rapid.IntRange(0, 64).Draw(rt, "pad")
			blob = make([]byte, 0, len(header)+pad)
			blob = append(blob, header...)
			blob = append(blob, make([]byte, pad)...)
		}

		// Ground-truth decision, computed independently of the repo. Size is
		// checked first (Req 17.3): an oversize blob is too-large regardless of
		// format. Note StoreImage applies the size check before the sniff, so
		// the expected sentinel for an oversize-and-unsupported blob is
		// ErrImageTooLarge.
		oversize := len(blob) > MaxImageBytes
		_, sniffOK := sniffImageMIME(blob)
		wantAccept := !oversize && sniffOK

		before := countImages(t, f)

		ref, err := repo.StoreImage(ctx, f.rcA, blob)

		after := countImages(t, f)

		if wantAccept {
			if err != nil {
				rt.Fatalf("valid paste (len=%d) should be accepted, got error: %v", len(blob), err)
			}
			if !strings.HasPrefix(ref, imageRefScheme) {
				rt.Fatalf("accepted paste reference = %q, want %s prefix", ref, imageRefScheme)
			}
			if after != before+1 {
				rt.Fatalf("accepted paste should store exactly one blob: before=%d after=%d", before, after)
			}
		} else {
			if ref != "" {
				rt.Fatalf("rejected paste must return no reference, got %q", ref)
			}
			// Distinct, correct sentinel: too-large takes precedence over
			// unsupported-format, matching StoreImage's ordering.
			if oversize {
				if !errors.Is(err, ErrImageTooLarge) {
					rt.Fatalf("oversize paste (len=%d) should return ErrImageTooLarge, got: %v", len(blob), err)
				}
			} else {
				if !errors.Is(err, ErrUnsupportedImageFormat) {
					rt.Fatalf("unsupported paste should return ErrUnsupportedImageFormat, got: %v", err)
				}
			}
			if after != before {
				rt.Fatalf("rejected paste must store nothing (content unchanged): before=%d after=%d", before, after)
			}
		}
	})

	// Deterministic boundary coverage: the 10 MB bound is inclusive, so a valid
	// image of exactly MaxImageBytes is accepted while MaxImageBytes+1 is
	// rejected as too-large — asserted here so the frontier is covered
	// regardless of what the sampler drew above.
	t.Run("boundary", func(t *testing.T) {
		before := countImages(t, f)

		atLimit := make([]byte, MaxImageBytes)
		copy(atLimit, pngBytes)
		ref, err := repo.StoreImage(ctx, f.rcA, atLimit)
		if err != nil {
			t.Fatalf("paste at exactly MaxImageBytes should be accepted, got: %v", err)
		}
		if !strings.HasPrefix(ref, imageRefScheme) {
			t.Fatalf("at-limit reference = %q, want %s prefix", ref, imageRefScheme)
		}
		if got := countImages(t, f); got != before+1 {
			t.Fatalf("at-limit paste should store exactly one blob: before=%d after=%d", before, got)
		}

		before = countImages(t, f)
		overLimit := make([]byte, MaxImageBytes+1)
		copy(overLimit, pngBytes)
		if _, err := repo.StoreImage(ctx, f.rcA, overLimit); !errors.Is(err, ErrImageTooLarge) {
			t.Fatalf("paste at MaxImageBytes+1 should return ErrImageTooLarge, got: %v", err)
		}
		if got := countImages(t, f); got != before {
			t.Fatalf("over-limit paste must store nothing: before=%d after=%d", before, got)
		}
	})
}
