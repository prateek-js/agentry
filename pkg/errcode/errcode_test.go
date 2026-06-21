package errcode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestErrorRendersCodeAndMessage(t *testing.T) {
	e := New(SandboxNotFound, "sandbox %q vanished", "sb1")
	if got := e.Error(); !strings.Contains(got, "S110") || !strings.Contains(got, "sb1") {
		t.Errorf("Error() = %q; want code + sandbox-id", got)
	}
}

func TestErrorWithDetails(t *testing.T) {
	e := New(BindingForbidden, "denied").WithDetails(map[string]any{
		"service": "trino",
		"reason":  "not in catalog",
	})
	if e.Details["service"] != "trino" {
		t.Errorf("Details missing service")
	}
}

func TestHTTPStatusMapping(t *testing.T) {
	cases := []struct {
		code string
		want int
	}{
		{SandboxInvalidRequest, http.StatusBadRequest},          // S001 → 400
		{SandboxNotFound, http.StatusNotFound},                  // S110 → 404
		{SandboxAlreadyExists, http.StatusConflict},             // S210 → 409
		{ProjectDependsUnresolved, http.StatusFailedDependency}, // P310 → 424
		{BindingForbidden, http.StatusForbidden},                // B400 → 403
		{Internal, http.StatusInternalServerError},              // Z900 → 500
	}
	for _, c := range cases {
		got := (&Error{Code: c.code}).HTTPStatus()
		if got != c.want {
			t.Errorf("code %s → status %d; want %d", c.code, got, c.want)
		}
	}
}

func TestWriteJSONRoundTrips(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSON(w, New(SandboxNotFound, "no sandbox %q", "sb1").WithDetails(map[string]any{
		"sandbox_id": "sb1",
	}))

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", w.Code)
	}
	parsed, err := FromHTTPResponse(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Code != SandboxNotFound {
		t.Errorf("parsed code = %s; want %s", parsed.Code, SandboxNotFound)
	}
	if parsed.Details["sandbox_id"] != "sb1" {
		t.Errorf("details lost: %+v", parsed.Details)
	}
}

func TestIsCompareByCode(t *testing.T) {
	a := New(SandboxNotFound, "x")
	b := &Error{Code: SandboxNotFound}
	if !a.Is(b) {
		t.Error("two errors with same code should match Is")
	}
	c := &Error{Code: BindingForbidden}
	if a.Is(c) {
		t.Error("different codes should not match")
	}
}

func TestFromHTTPResponseRejectsNonCoded(t *testing.T) {
	raw, _ := json.Marshal(map[string]string{"message": "oops"})
	_, err := FromHTTPResponse(raw)
	if err == nil {
		t.Error("expected error for body without code field")
	}
}
