package repo

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// These tests add net-new coverage on top of image_test.go and
// image_property_test.go, which already assert that StoreImage returns the
// distinct sentinels ErrUnsupportedImageFormat (Req 17.2) and ErrImageTooLarge
// (Req 17.3) and stores nothing on rejection. What was NOT yet asserted is that
// the two rejection paths carry DISTINCT, non-empty error MESSAGES so the editor
// can surface the right text to the user: "unsupported format" (Req 17.2) versus
// "exceeds the maximum allowed size" (Req 17.3). A user sees a message, not a
// sentinel, so the messages themselves must differ and each rejection path must
// return the message matching its cause.
//
// Validates: Requirements 17.2, 17.3

// TestImageRejectionSentinelsAndMessagesDistinct confirms the two rejection
// sentinels are distinct values AND that their Error() strings are non-empty and
// distinct from each other, so the two failure causes are never conflated in the
// message a user would see (Req 17.2 unsupported vs Req 17.3 too-large).
func TestImageRejectionSentinelsAndMessagesDistinct(t *testing.T) {
	if ErrUnsupportedImageFormat == ErrImageTooLarge {
		t.Fatal("ErrUnsupportedImageFormat and ErrImageTooLarge must be distinct sentinels")
	}

	unsupportedMsg := ErrUnsupportedImageFormat.Error()
	tooLargeMsg := ErrImageTooLarge.Error()

	if unsupportedMsg == "" {
		t.Error("ErrUnsupportedImageFormat.Error() must be non-empty (Req 17.2)")
	}
	if tooLargeMsg == "" {
		t.Error("ErrImageTooLarge.Error() must be non-empty (Req 17.3)")
	}
	if unsupportedMsg == tooLargeMsg {
		t.Errorf("rejection messages must be distinct so users get the right message; both are %q", unsupportedMsg)
	}
}

// TestStoreImageRejectionMessagesMatchCause confirms each rejection PATH returns
// the message matching its cause: an unsupported-format paste yields a message
// equal to ErrUnsupportedImageFormat's (Req 17.2), and an oversize paste yields a
// message that carries ErrImageTooLarge's text (Req 17.3) — and never the other
// cause's message. This closes the gap between "distinct sentinels" (already
// covered) and "distinct messages surfaced on the right path".
func TestStoreImageRejectionMessagesMatchCause(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &ImageRepo{Base: f.base}
	ctx := context.Background()

	unsupportedMsg := ErrUnsupportedImageFormat.Error()
	tooLargeMsg := ErrImageTooLarge.Error()

	t.Run("unsupported format path", func(t *testing.T) {
		_, err := repo.StoreImage(ctx, f.rcA, []byte("this is not an image at all, just text"))
		if err == nil {
			t.Fatal("expected rejection for unsupported format")
		}
		if !errors.Is(err, ErrUnsupportedImageFormat) {
			t.Fatalf("got %v, want ErrUnsupportedImageFormat", err)
		}
		// The user-facing message must be the unsupported-format message
		// (Req 17.2), never the too-large one (Req 17.3).
		if !strings.Contains(err.Error(), unsupportedMsg) {
			t.Errorf("error message %q does not carry the unsupported-format message %q", err.Error(), unsupportedMsg)
		}
		if strings.Contains(err.Error(), tooLargeMsg) {
			t.Errorf("unsupported-format path leaked the too-large message: %q", err.Error())
		}
	})

	t.Run("too large path", func(t *testing.T) {
		// A valid PNG header padded past the 10 MB limit: the cause is size,
		// not format, so the message must be the too-large one.
		oversize := append(append([]byte{}, pngBytes...), make([]byte, MaxImageBytes)...)
		_, err := repo.StoreImage(ctx, f.rcA, oversize)
		if err == nil {
			t.Fatal("expected rejection for oversize paste")
		}
		if !errors.Is(err, ErrImageTooLarge) {
			t.Fatalf("got %v, want ErrImageTooLarge", err)
		}
		if !strings.Contains(err.Error(), tooLargeMsg) {
			t.Errorf("error message %q does not carry the too-large message %q", err.Error(), tooLargeMsg)
		}
		if strings.Contains(err.Error(), unsupportedMsg) {
			t.Errorf("too-large path leaked the unsupported-format message: %q", err.Error())
		}
	})
}
