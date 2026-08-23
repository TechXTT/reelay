package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// Error codes. Stable strings — the UI branches on these, not on prose.
const (
	CodeBadRequest   = "bad_request"
	CodeUnauthorized = "unauthorized"
	CodeNotFound     = "not_found"
	CodeConflict     = "conflict"
	CodeUnavailable  = "unavailable"
	CodeInternal     = "internal"
)

// Error is the single response shape for every failure:
//
//	{"error": {"code": "...", "message": "...", "details": {...}}}
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`

	// status is the HTTP status to send; not serialised.
	status int
	// cause is wrapped for the server log and never shown to the client.
	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

func (e *Error) WithDetail(key string, value any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = value
	return e
}

func (e *Error) WithCause(err error) *Error {
	e.cause = err
	return e
}

func BadRequest(format string, args ...any) *Error {
	return &Error{Code: CodeBadRequest, Message: fmt.Sprintf(format, args...), status: http.StatusBadRequest}
}

func Unauthorized(message string) *Error {
	return &Error{Code: CodeUnauthorized, Message: message, status: http.StatusUnauthorized}
}

func NotFound(format string, args ...any) *Error {
	return &Error{Code: CodeNotFound, Message: fmt.Sprintf(format, args...), status: http.StatusNotFound}
}

func Conflict(format string, args ...any) *Error {
	return &Error{Code: CodeConflict, Message: fmt.Sprintf(format, args...), status: http.StatusConflict}
}

func Unavailable(format string, args ...any) *Error {
	return &Error{Code: CodeUnavailable, Message: fmt.Sprintf(format, args...), status: http.StatusServiceUnavailable}
}

// Internal deliberately does not accept a message: the client is told nothing
// beyond "internal", and the real error goes to the log with the request id.
func Internal(cause error) *Error {
	return &Error{
		Code:    CodeInternal,
		Message: "internal error",
		status:  http.StatusInternalServerError,
		cause:   cause,
	}
}

type errorEnvelope struct {
	Error *Error `json:"error"`
}

// writeJSON emits v with the given status. A late encoding failure cannot be
// converted into an error response — the status line is already on the wire —
// so it is logged instead.
func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		log.Error("marshal response body", "error", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"internal error"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		log.Debug("write response body", "error", err)
	}
}

// writeError renders any error as the envelope. Errors that are not *Error are
// treated as internal: leaking a raw error string to an HTTP client is how
// database paths and credentials escape.
func writeError(w http.ResponseWriter, log *slog.Logger, err error) {
	apiErr, ok := err.(*Error)
	if !ok {
		apiErr = Internal(err)
	}
	if apiErr.status == 0 {
		apiErr.status = http.StatusInternalServerError
	}
	if apiErr.status >= 500 {
		log.Error("request failed", "code", apiErr.Code, "error", apiErr.Error())
	} else {
		log.Debug("request rejected", "code", apiErr.Code, "message", apiErr.Message)
	}
	writeJSON(w, log, apiErr.status, errorEnvelope{Error: apiErr})
}

// handler is a http.HandlerFunc that may return an error, so handlers stop
// having to remember to write a response on every failure path.
type handler func(http.ResponseWriter, *http.Request) error

func (s *Server) wrap(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			writeError(w, s.logFor(r), err)
		}
	}
}
