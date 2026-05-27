// Package errcode is the stable error-code scheme MCP tools and HTTP
// handlers return so the LLM (and any other automated consumer) can
// react deterministically without parsing prose messages.
//
// Every error has the shape:
//
//	{"code": "B110", "message": "human-readable", "details": {...}}
//
// Code = one letter (the family) + three digits. Families:
//
//	S — Sandbox lifecycle (create / delete / list / not-found)
//	B — Bindings (service-bind, env, secrets)
//	D — Dev-deps (postgres-in-sandbox, redis-in-sandbox, …)
//	K — Skills (catalog, load, version)
//	P — Projects (start / stop / manifest)
//	R — Runtime ops (shell, file, code-exec)
//	V — Validation (bad request shape, missing field)
//	Z — Internal / unexpected
//
// 100s are "not found" / "missing target", 200s are "conflict / already
// exists", 300s are "precondition / dependency", 400s are "limit /
// quota / forbidden", 900s are wrapped internal errors. Skills cite
// the codes their op emits so the LLM knows which can be retried.
package errcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ----- Families ----------------------------------------------------------

// Sandbox lifecycle.
const (
	SandboxInvalidRequest = "S001"
	SandboxNotFound       = "S110"
	SandboxAlreadyExists  = "S210"
	SandboxQuotaExceeded  = "S400"
	SandboxInternal       = "S900"
)

// Bindings — service bindings, env vars, user secrets.
const (
	BindingInvalidRequest      = "B001"
	BindingServiceNotInCatalog = "B110"
	BindingMintFailed          = "B300"
	BindingForbidden           = "B400"
	BindingInternal            = "B900"

	SecretLooksLikeSecret = "B010" // for env::set rejection
	SecretNotFound        = "B120"
)

// Dev-deps — in-sandbox dependency runners.
const (
	DevDepInvalidRequest = "D001"
	DevDepNotInCatalog   = "D110"
	DevDepStartFailed    = "D300"
	DevDepAlreadyAdded   = "D210"
	DevDepInternal       = "D900"
)

// Skills — catalog, load, versioning.
const (
	SkillInvalidRequest    = "K001"
	SkillNotInCatalog      = "K110"
	SkillVersionUnknown    = "K120"
	SkillFetchFailed       = "K300"
	SkillInternal          = "K900"
)

// Projects — managed project manifests + lifecycle.
const (
	ProjectInvalidManifest  = "P001"
	ProjectNotFound         = "P110"
	ProjectAlreadyRunning   = "P210"
	ProjectStartFailed      = "P300"
	ProjectDependsUnresolved = "P310"
	ProjectInternal         = "P900"
)

// Runtime ops — shell / file / code-exec failures.
const (
	RuntimeInvalidRequest = "R001"
	RuntimeNotFound       = "R110"
	RuntimeExecFailed     = "R300"
	RuntimeForbidden      = "R400"
	RuntimeInternal       = "R900"
)

// Validation — generic input-shape errors.
const (
	InvalidRequest = "V001"
	MissingField   = "V010"
	InvalidValue   = "V020"
)

// Internal — wrapper for unexpected failures (panics, IO, etc).
const (
	Internal = "Z900"
)

// ----- Error type --------------------------------------------------------

// Error is the structured error all handlers / tools return.
// Implements error so callers can errors.Is and errors.As against it.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// New constructs a structured error.
func New(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithDetails attaches structured details to the error. Returns the
// receiver for fluent chaining.
func (e *Error) WithDetails(details map[string]any) *Error {
	e.Details = details
	return e
}

// Error renders code + message — the shape the LLM (or any reader)
// sees when the error is stringified by, e.g., MCP's error pipeline.
func (e *Error) Error() string {
	return e.Code + ": " + e.Message
}

// HTTPStatus maps an error code family to a sensible HTTP status.
// Used by HTTP handlers to write the right status alongside the
// structured body.
func (e *Error) HTTPStatus() int {
	if e == nil || len(e.Code) < 2 {
		return http.StatusInternalServerError
	}
	switch e.Code[1] {
	case '0', '1':
		switch e.Code[1] {
		case '0':
			return http.StatusBadRequest
		case '1':
			return http.StatusNotFound
		}
	case '2':
		return http.StatusConflict
	case '3':
		return http.StatusFailedDependency
	case '4':
		return http.StatusForbidden
	case '9':
		return http.StatusInternalServerError
	}
	return http.StatusInternalServerError
}

// WriteJSON writes the error as JSON to w with the right status. Use
// in HTTP handlers as `errcode.WriteJSON(w, errcode.New(...))`.
func WriteJSON(w http.ResponseWriter, err *Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(err.HTTPStatus())
	_ = json.NewEncoder(w).Encode(err)
}

// FromHTTPResponse parses an error JSON body (as written by WriteJSON)
// back into an *Error. Returns nil, nil if the body is empty or
// doesn't look like a coded error — useful for clients that
// distinguish coded errors from network errors.
func FromHTTPResponse(body []byte) (*Error, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var e Error
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, err
	}
	if e.Code == "" {
		return nil, errors.New("response body has no code field")
	}
	return &e, nil
}

// Is supports errors.Is(err, errcode.SandboxNotFound) style matching
// against bare code strings — the most common shape callers want.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return e.Code == t.Code
}
