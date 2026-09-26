package api

// Tests for the standard error envelope (task 22.2). The REST layer answers
// every failure with exactly one shape — { "error": { "code", "message" } } —
// and maps each domain error sentinel the service/repo layers raise to a fixed
// (HTTP status, machine code) pair (design.md — "Standard error envelope" and
// the "Error Handling" categories). These two guarantees are what a client
// relies on to branch on a stable code rather than parsing prose, so they are
// pinned here:
//
//   - TestWriteServiceErrorMapping is table-driven over every sentinel
//     writeServiceError recognizes, asserting the documented status+code and
//     that the JSON body is exactly {"error":{"code":...,"message":...}} with
//     no extra fields and a non-empty message.
//   - TestWriteErrorEnvelopeShape pins the raw envelope writer's shape and
//     headers independently of any mapping.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/influence/influence/internal/repo"
	"github.com/influence/influence/internal/service"
)

// decodeEnvelopeExact decodes a response body into the exact envelope shape and
// fails if the body carries any field outside {"error":{"code","message"}}.
// Using DisallowUnknownFields makes the test assert the shape is EXACTLY the
// documented envelope, not merely a superset of it.
func decodeEnvelopeExact(t *testing.T, rec *httptest.ResponseRecorder) errorPayload {
	t.Helper()

	// Outer object must have only "error".
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &outer); err != nil {
		t.Fatalf("body is not a JSON object: %q (%v)", rec.Body.String(), err)
	}
	if len(outer) != 1 {
		t.Fatalf("envelope has %d top-level fields, want exactly 1 (\"error\"): %q", len(outer), rec.Body.String())
	}
	raw, ok := outer["error"]
	if !ok {
		t.Fatalf("envelope missing \"error\" field: %q", rec.Body.String())
	}

	// Inner object must have only "code" and "message".
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var payload errorPayload
	if err := dec.Decode(&payload); err != nil {
		t.Fatalf("inner error object has unexpected shape: %q (%v)", string(raw), err)
	}
	return payload
}

// TestWriteServiceErrorMapping asserts every domain sentinel writeServiceError
// recognizes maps to the documented (status, code) pair and that the body is
// exactly the standard envelope with a non-empty message.
func TestWriteServiceErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "repo not accessible -> tenant isolation",
			err:        repo.ErrNotAccessible,
			wantStatus: http.StatusNotFound,
			wantCode:   codeTenantIsolation,
		},
		{
			name:       "service polygon not accessible -> tenant isolation",
			err:        service.ErrPolygonNotAccessible,
			wantStatus: http.StatusNotFound,
			wantCode:   codeTenantIsolation,
		},
		{
			name:       "repo validation -> validation",
			err:        fmt.Errorf("%w: name empty", repo.ErrValidation),
			wantStatus: http.StatusBadRequest,
			wantCode:   codeValidation,
		},
		{
			name:       "service invalid span -> validation",
			err:        service.ErrInvalidSpan,
			wantStatus: http.StatusBadRequest,
			wantCode:   codeValidation,
		},
		{
			name:       "repo hierarchy -> hierarchy",
			err:        fmt.Errorf("%w: cycle", repo.ErrHierarchy),
			wantStatus: http.StatusConflict,
			wantCode:   codeHierarchy,
		},
		{
			name:       "service reveal denied -> reveal denied",
			err:        service.ErrRevealDenied,
			wantStatus: http.StatusForbidden,
			wantCode:   codeRevealDenied,
		},
		{
			name:       "service reveal failed -> reveal failed",
			err:        service.ErrRevealFailed,
			wantStatus: http.StatusConflict,
			wantCode:   codeRevealFailed,
		},
		{
			name:       "repo unsupported image format -> validation",
			err:        repo.ErrUnsupportedImageFormat,
			wantStatus: http.StatusBadRequest,
			wantCode:   codeValidation,
		},
		{
			name:       "repo image too large -> validation",
			err:        repo.ErrImageTooLarge,
			wantStatus: http.StatusBadRequest,
			wantCode:   codeValidation,
		},
		{
			name:       "repo locked -> locked",
			err:        repo.ErrLocked{HolderUserID: 42},
			wantStatus: http.StatusConflict,
			wantCode:   codeLocked,
		},
		{
			name:       "wrapped locked still maps to locked",
			err:        fmt.Errorf("save rejected: %w", repo.ErrLocked{HolderUserID: 7}),
			wantStatus: http.StatusConflict,
			wantCode:   codeLocked,
		},
		{
			name:       "unknown error -> internal",
			err:        fmt.Errorf("something unexpected happened"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   codeInternal,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeServiceError(rec, tc.err)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("content-type = %q, want application/json; charset=utf-8", ct)
			}

			payload := decodeEnvelopeExact(t, rec)
			if payload.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", payload.Code, tc.wantCode)
			}
			if payload.Message == "" {
				t.Errorf("message is empty; the envelope must always carry a human-readable message")
			}
		})
	}
}

// TestWriteErrorEnvelopeShape pins the raw writer independently of the mapping:
// the body is exactly {"error":{"code","message"}} with the given values, the
// status is honored, and the JSON content type is set.
func TestWriteErrorEnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusTeapot, "SOME_CODE", "some message")

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q, want application/json; charset=utf-8", ct)
	}
	payload := decodeEnvelopeExact(t, rec)
	if payload.Code != "SOME_CODE" || payload.Message != "some message" {
		t.Errorf("payload = %+v, want {SOME_CODE some message}", payload)
	}
}
