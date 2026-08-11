package pocketcaddy

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// This file defines the request-scoped error type. Startup errors -- a
// malformed directive, an unreadable database -- are plain errors: they surface
// through Caddy's config validation, never over HTTP, so they have no status
// and no code.
//
// The wire shape borrows JSON:API's error vocabulary (status/code/title/detail/
// source/meta) without its document envelope. A JSON:API document wraps the
// whole response in a top-level object, which a JSONL stream cannot do: rows
// are already flushed by the time a mid-stream failure is known, so the error
// has to be one more line rather than a wrapper around the preceding ones.
// Using the field names alone keeps both failure paths identical in shape,
// which is what a client actually branches on.

// Code is a stable, machine-readable classification of a request failure.
// Clients branch on these; the accompanying detail is prose and may change.
type Code string

const (
	// Request shape.
	CodeMethodNotAllowed Code = "method_not_allowed"
	CodeUnsupportedMedia Code = "unsupported_media_type"
	CodeInvalidBody      Code = "invalid_body"
	CodeBodyTooLarge     Code = "body_too_large"

	// Parameters.
	CodeUnknownParameter Code = "unknown_parameter"
	CodeMissingParameter Code = "missing_parameter"
	CodeInvalidType      Code = "invalid_type"

	// PostgREST-style shaping.
	CodeUnknownColumn   Code = "unknown_column"
	CodeInvalidColumn   Code = "invalid_column"
	CodeUnknownOperator Code = "unknown_operator"
	CodeInvalidFilter   Code = "invalid_filter"
	CodeInvalidOrder    Code = "invalid_order"
	CodeInvalidSelect   Code = "invalid_select"
	CodeInvalidRange    Code = "invalid_range"

	// Execution.
	CodeQueryFailed Code = "query_failed"
	CodeTimeout     Code = "timeout"
)

// title returns the human-readable summary for a code. It is fixed per code --
// unlike detail, which varies with the offending value -- so clients may group
// on either.
func (c Code) title() string {
	switch c {
	case CodeMethodNotAllowed:
		return "Method not allowed"
	case CodeUnsupportedMedia:
		return "Unsupported media type"
	case CodeInvalidBody:
		return "Invalid request body"
	case CodeBodyTooLarge:
		return "Request body too large"
	case CodeUnknownParameter:
		return "Unknown parameter"
	case CodeMissingParameter:
		return "Missing required parameter"
	case CodeInvalidType:
		return "Invalid parameter value"
	case CodeUnknownColumn:
		return "Unknown column"
	case CodeInvalidColumn:
		return "Invalid column name"
	case CodeUnknownOperator:
		return "Unknown filter operator"
	case CodeInvalidFilter:
		return "Invalid filter"
	case CodeInvalidOrder:
		return "Invalid order term"
	case CodeInvalidSelect:
		return "Invalid select term"
	case CodeInvalidRange:
		return "Invalid range"
	case CodeQueryFailed:
		return "Query execution failed"
	case CodeTimeout:
		return "Query timed out"
	}
	return "Error"
}

// source locates the request input that caused an error. The field names follow
// JSON:API: parameter for a query-string key, pointer for a JSON Pointer into
// the request body.
type source struct {
	Parameter string `json:"parameter,omitempty"`
	Pointer   string `json:"pointer,omitempty"`
}

// Error is a request-scoped failure with everything needed to render it: a
// status for the response line and a code, title, detail and source for the
// body.
type Error struct {
	Code   Code
	Status int
	Detail string
	Source source
	// Meta carries per-error extras, such as the partial-response markers on a
	// mid-stream failure.
	Meta map[string]any

	err error // wrapped cause, for errors.Is/As; never rendered
}

func (e *Error) Error() string { return e.Detail }
func (e *Error) Unwrap() error { return e.err }

// wire renders the error as the object written to the response. status is a
// string because JSON:API specifies it that way, and it is omitted once the
// response is committed, where it would be a lie.
func (e *Error) wire() map[string]any {
	obj := map[string]any{
		"code":   string(e.Code),
		"title":  e.Code.title(),
		"detail": e.Detail,
	}
	if e.Status != 0 {
		obj["status"] = strconv.Itoa(e.Status)
	}
	if e.Source != (source{}) {
		src := map[string]any{}
		if e.Source.Parameter != "" {
			src["parameter"] = e.Source.Parameter
		}
		if e.Source.Pointer != "" {
			src["pointer"] = e.Source.Pointer
		}
		obj["source"] = src
	}
	if len(e.Meta) > 0 {
		obj["meta"] = e.Meta
	}
	return map[string]any{"error": obj}
}

// withParam attaches the query-string parameter that caused the error, unless
// a source is already set by a more specific caller.
func (e *Error) withParam(name string) *Error {
	if e.Source == (source{}) {
		e.Source = source{Parameter: name}
	}
	return e
}

// withPointer attaches a JSON Pointer into the request body, replacing any
// parameter source. A constructor cannot know whether the value arrived in the
// query string or the body, so it defaults to parameter and the body path
// overrides it here.
func (e *Error) withPointer(ptr string) *Error {
	e.Source = source{Pointer: ptr}
	return e
}

// newError builds a request error. Most callers should use one of the
// constructors below rather than calling this directly.
func newError(status int, code Code, format string, args ...any) *Error {
	e := &Error{Code: code, Status: status, Detail: fmt.Sprintf(format, args...)}
	// Preserve a wrapped cause so errors.Is/As still reach it.
	for _, a := range args {
		if cause, ok := a.(error); ok {
			e.err = cause
			break
		}
	}
	return e
}

// badRequest builds a 400.
func badRequest(code Code, format string, args ...any) *Error {
	return newError(http.StatusBadRequest, code, format, args...)
}

// --- parameter errors --------------------------------------------------------

func errUnknownParam(name, queryName string, declared []string) *Error {
	e := badRequest(CodeUnknownParameter, "query %q has no parameter %q", queryName, name)
	e.Source = source{Parameter: name}
	if len(declared) > 0 {
		e.Meta = map[string]any{"declared": declared}
	}
	return e
}

func errMissingParam(name string) *Error {
	e := badRequest(CodeMissingParameter, "parameter %q is required", name)
	e.Source = source{Parameter: name}
	return e
}

// errInvalidType reports a value that does not fit its declared type. cause is
// the underlying conversion error, wrapped but not rendered.
func errInvalidType(name string, cause error) *Error {
	e := badRequest(CodeInvalidType, "parameter %q: %v", name, cause)
	e.Source = source{Parameter: name}
	e.err = cause
	return e
}

// --- shaping errors ----------------------------------------------------------

// errUnknownColumn reports a column the query does not return. The source is
// left for the caller to set: a column reaches here from a filter (where the
// query-string key *is* the column) or from within an order/select spec (where
// the key is "order"/"select" and the column is only part of the value).
func errUnknownColumn(column, queryName string, known []string) *Error {
	e := badRequest(CodeUnknownColumn, "query %q has no column %q", queryName, column)
	e.Meta = map[string]any{"column": column}
	if len(known) > 0 {
		e.Meta["columns"] = known
	}
	return e
}

func errInvalidColumn(column string) *Error {
	e := badRequest(CodeInvalidColumn, "invalid column name %q", column)
	e.Meta = map[string]any{"column": column}
	return e
}

func errUnknownOperator(op, column string) *Error {
	e := badRequest(CodeUnknownOperator, "unknown filter operator %q", op)
	e.Source = source{Parameter: column}
	e.Meta = map[string]any{"operators": knownOperators()}
	return e
}

// knownOperators lists every operator token a filter accepts, for the hint on
// an unknown-operator error.
func knownOperators() []string {
	out := make([]string, 0, len(operators)+2)
	for op := range operators {
		out = append(out, op)
	}
	out = append(out, "is", "in")
	sortStrings(out)
	return out
}

// sortStrings is an insertion sort, used to keep hint lists stable in output
// without pulling in sort for a handful of elements.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// --- execution errors --------------------------------------------------------

// errQueryFailed reports a failure from the database. The driver's message is
// included: these queries are operator-authored and the databases are read-only,
// so the detail is diagnostic rather than a disclosure of untrusted schema.
func errQueryFailed(cause error) *Error {
	return &Error{
		Code:   CodeQueryFailed,
		Status: http.StatusInternalServerError,
		Detail: cleanDriverMessage(cause),
		err:    cause,
	}
}

func errTimeout(cause error) *Error {
	return &Error{
		Code:   CodeTimeout,
		Status: http.StatusGatewayTimeout,
		Detail: "query exceeded the configured timeout",
		err:    cause,
	}
}

// cleanDriverMessage strips the driver's redundant prefix so the detail reads
// as the SQLite message alone.
func cleanDriverMessage(err error) string {
	msg := err.Error()
	for _, prefix := range []string{"SQL logic error: ", "sqlite3: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	return msg
}
