package pocketcaddy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// wireError is the decoded error line, mirroring the shape clients parse.
type wireError struct {
	Error struct {
		Status string `json:"status"`
		Code   string `json:"code"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
		Source struct {
			Parameter string `json:"parameter"`
			Pointer   string `json:"pointer"`
		} `json:"source"`
		Meta map[string]any `json:"meta"`
	} `json:"error"`
}

// getError issues a GET and decodes the single error line it returns.
func getError(t *testing.T, h *Handler, target string) (*http.Response, wireError) {
	t.Helper()
	resp, body := serve(t, h, target)

	var we wireError
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &we); err != nil {
		t.Fatalf("decode error line %q: %v", body, err)
	}
	return resp, we
}

// postError issues a POST with a JSON body and decodes the error line.
func postError(t *testing.T, h *Handler, target, body string) (*http.Response, wireError) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })
		if err := h.ServeHTTP(w, r, next); err != nil {
			t.Errorf("ServeHTTP: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+target, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	var we wireError
	if err := json.NewDecoder(resp.Body).Decode(&we); err != nil {
		t.Fatalf("decode error line: %v", err)
	}
	return resp, we
}

func TestErrorCarriesCodeStatusAndSource(t *testing.T) {
	h := newTrailerTestHandler(t, 3, 0)

	resp, we := getError(t, h, "/nums?id=gtt.2")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", resp.StatusCode)
	}
	if we.Error.Code != string(CodeUnknownOperator) {
		t.Errorf("code: want %q, got %q", CodeUnknownOperator, we.Error.Code)
	}
	// JSON:API renders status as a string, inside the error object.
	if we.Error.Status != "400" {
		t.Errorf("status field: want \"400\", got %q", we.Error.Status)
	}
	if we.Error.Title != CodeUnknownOperator.title() {
		t.Errorf("title: want %q, got %q", CodeUnknownOperator.title(), we.Error.Title)
	}
	if we.Error.Source.Parameter != "id" {
		t.Errorf("source.parameter: want \"id\", got %q", we.Error.Source.Parameter)
	}
	// The hint lists what would have been accepted.
	ops, ok := we.Error.Meta["operators"].([]any)
	if !ok || len(ops) == 0 {
		t.Fatalf("want an operators hint, got %v", we.Error.Meta)
	}
}

func TestErrorUsesNoUnderscorePrefixedKeys(t *testing.T) {
	h := newTrailerTestHandler(t, 3, 0)
	_, body := serve(t, h, "/nums?limit=-1")

	var raw map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["error"]; !ok {
		t.Fatalf("want a top-level \"error\" key, got %v", raw)
	}
	for k := range raw {
		if strings.HasPrefix(k, "_") {
			t.Errorf("underscore-prefixed key %q should be gone", k)
		}
	}
}

func TestUnknownColumnHintsAvailableColumns(t *testing.T) {
	h := newTrailerTestHandler(t, 3, 0)

	resp, we := getError(t, h, "/nums?order=nope.desc")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", resp.StatusCode)
	}
	if we.Error.Code != string(CodeUnknownColumn) {
		t.Errorf("code: want %q, got %q", CodeUnknownColumn, we.Error.Code)
	}
	cols, ok := we.Error.Meta["columns"].([]any)
	if !ok || len(cols) == 0 {
		t.Fatalf("want a columns hint, got %v", we.Error.Meta)
	}
	if cols[0] != "id" {
		t.Errorf("want the query's real column, got %v", cols)
	}
}

// source.parameter is the query-string key the client actually sent. For a
// filter that is the column; inside an order/select spec the column is only
// part of the value, so it belongs in meta instead.
func TestColumnFaultSourceNamesTheQueryStringKey(t *testing.T) {
	h := newTrailerTestHandler(t, 3, 0)

	for _, tc := range []struct{ target, param, column string }{
		{"/nums?order=nope.desc", "order", "nope"},
		{"/nums?select=bogus", "select", "bogus"},
		{"/nums?nope=eq.1", "nope", "nope"}, // a filter keys on the column
	} {
		_, we := getError(t, h, tc.target)
		if we.Error.Source.Parameter != tc.param {
			t.Errorf("%s: source.parameter want %q, got %q",
				tc.target, tc.param, we.Error.Source.Parameter)
		}
		if got := we.Error.Meta["column"]; got != tc.column {
			t.Errorf("%s: meta.column want %q, got %v", tc.target, tc.column, got)
		}
	}
}

func TestBodyErrorSourceIsAJSONPointer(t *testing.T) {
	h := newTrailerTestHandler(t, 3, 0)

	resp, we := postError(t, h, "/nums", `{"nope": 1}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", resp.StatusCode)
	}
	if we.Error.Code != string(CodeUnknownParameter) {
		t.Errorf("code: want %q, got %q", CodeUnknownParameter, we.Error.Code)
	}
	// A body-sourced fault points into the document, not at a query key.
	if we.Error.Source.Pointer != "/nope" {
		t.Errorf("source.pointer: want \"/nope\", got %q", we.Error.Source.Pointer)
	}
	if we.Error.Source.Parameter != "" {
		t.Errorf("a body fault should not set source.parameter, got %q", we.Error.Source.Parameter)
	}
}

func TestMethodNotAllowedCarriesCode(t *testing.T) {
	h := newTrailerTestHandler(t, 3, 0)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })
		if err := h.ServeHTTP(w, r, next); err != nil {
			t.Errorf("ServeHTTP: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/nums", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status: want 405, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD, POST" {
		t.Errorf("Allow: want \"GET, HEAD, POST\", got %q", got)
	}

	var we wireError
	if err := json.NewDecoder(resp.Body).Decode(&we); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if we.Error.Code != string(CodeMethodNotAllowed) {
		t.Errorf("code: want %q, got %q", CodeMethodNotAllowed, we.Error.Code)
	}
}

// Errors keep the row stream's content type: one parser handles the whole
// response, and a mid-stream error could not change the header anyway.
func TestErrorKeepsNDJSONContentType(t *testing.T) {
	h := newTrailerTestHandler(t, 3, 0)

	resp, _ := getError(t, h, "/nums?limit=abc")
	if got := resp.Header.Get("Content-Type"); got != contentTypeJSONL {
		t.Errorf("Content-Type: want %q, got %q", contentTypeJSONL, got)
	}
}

func TestErrorLineIsValidJSONL(t *testing.T) {
	h := newTrailerTestHandler(t, 3, 0)
	_, body := serve(t, h, "/nums?limit=abc")

	// Exactly one line, newline-terminated, like every other record.
	if !strings.HasSuffix(body, "\n") {
		t.Errorf("error record must end in a newline, got %q", body)
	}
	if n := strings.Count(strings.TrimSpace(body), "\n"); n != 0 {
		t.Errorf("want a single line, got %d extra newlines", n)
	}
}

// A mid-stream failure uses the same error object as a pre-stream one, so a
// client needs a single check for both. It carries no status -- the client
// already received a 200 -- and marks the response partial.
func TestMidStreamErrorMatchesPreStreamShape(t *testing.T) {
	h := newTrailerTestHandler(t, 3, 0)

	w := httptest.NewRecorder()
	h.failMidStream(w, "nums", 2, errQueryFailed(&plainErr{"disk I/O error"}))

	var we wireError
	if err := json.Unmarshal([]byte(strings.TrimSpace(w.Body.String())), &we); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if we.Error.Code != string(CodeQueryFailed) {
		t.Errorf("code: want %q, got %q", CodeQueryFailed, we.Error.Code)
	}
	if we.Error.Detail != "disk I/O error" {
		t.Errorf("detail should carry the driver message, got %q", we.Error.Detail)
	}
	// Status would be a lie: the 200 is already on the wire.
	if we.Error.Status != "" {
		t.Errorf("mid-stream error must omit status, got %q", we.Error.Status)
	}
	if we.Error.Meta["partial"] != true {
		t.Errorf("want meta.partial true, got %v", we.Error.Meta)
	}
	if we.Error.Meta["rows"] != float64(2) {
		t.Errorf("want meta.rows 2, got %v", we.Error.Meta["rows"])
	}
}

// An unclassified error must not leak an arbitrary Go message as a coded
// detail; it becomes a generic 500.
func TestUnclassifiedErrorBecomesGeneric500(t *testing.T) {
	e := asError(errUnexpected())
	if e.Status != http.StatusInternalServerError {
		t.Errorf("status: want 500, got %d", e.Status)
	}
	if e.Detail != "internal error" {
		t.Errorf("detail: want a generic message, got %q", e.Detail)
	}
	if !strings.Contains(e.Unwrap().Error(), "boom") {
		t.Errorf("cause should still be reachable for logging, got %v", e.Unwrap())
	}
}

func errUnexpected() error { return &plainErr{"boom"} }

type plainErr struct{ msg string }

func (e *plainErr) Error() string { return e.msg }

// A code's title is fixed, so clients may group on it; the detail varies with
// the offending value.
func TestEveryCodeHasATitle(t *testing.T) {
	codes := []Code{
		CodeMethodNotAllowed, CodeUnsupportedMedia, CodeInvalidBody, CodeBodyTooLarge,
		CodeUnknownParameter, CodeMissingParameter, CodeInvalidType,
		CodeUnknownColumn, CodeInvalidColumn, CodeUnknownOperator, CodeInvalidFilter,
		CodeInvalidOrder, CodeInvalidSelect, CodeInvalidRange,
		CodeQueryFailed, CodeTimeout,
	}
	for _, c := range codes {
		if c.title() == "Error" {
			t.Errorf("code %q falls through to the default title", c)
		}
	}
}
