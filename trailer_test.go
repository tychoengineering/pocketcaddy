package pocketcaddy

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// newTrailerTestHandler builds a handler over a temp database holding n rows,
// served by a single query "nums" selecting them in order.
func newTrailerTestHandler(t *testing.T, n int, maxRows int64) *Handler {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.sqlite")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE nums (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= n; i++ {
		if _, err := db.Exec(`INSERT INTO nums (id) VALUES (?)`, i); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	sqlDir := filepath.Join(dir, "sql")
	if err := os.MkdirAll(sqlDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	query := "-- @db app\nSELECT id FROM nums ORDER BY id;\n"
	if err := os.WriteFile(filepath.Join(sqlDir, "nums.sql"), []byte(query), 0o644); err != nil {
		t.Fatalf("write query: %v", err)
	}

	st, err := openStore(dir, sqlDir, 2)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Provision does this in production; without it Columns is empty and every
	// filter/order/select column check silently passes.
	if err := st.validate(context.Background()); err != nil {
		t.Fatalf("validate: %v", err)
	}

	return &Handler{
		MaxRows:      maxRows,
		TimeoutSec:   30,
		MaxBodyBytes: defaultBodyLimit,
		store:        st,
		log:          zap.NewNop(),
	}
}

// serve runs the handler behind a real HTTP server so trailers go through
// actual chunked-encoding framing rather than a recorder's in-memory map.
func serve(t *testing.T, h *Handler, target string) (*http.Response, string) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })
		if err := h.ServeHTTP(w, r, next); err != nil {
			t.Errorf("ServeHTTP: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + target)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	// Trailers are only populated once the body is fully consumed.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(body)
}

func TestContentRangeTrailerReportsRowSpan(t *testing.T) {
	h := newTrailerTestHandler(t, 5, 1000)
	resp, body := serve(t, h, "/nums")

	if got := len(strings.Split(strings.TrimSpace(body), "\n")); got != 5 {
		t.Fatalf("row count = %d, want 5", got)
	}
	if got := resp.Trailer.Get("Content-Range"); got != "0-4/*" {
		t.Errorf("Content-Range = %q, want %q", got, "0-4/*")
	}
	if got := resp.Trailer.Get("X-Truncated"); got != "" {
		t.Errorf("X-Truncated = %q, want empty", got)
	}
}

func TestContentRangeTrailerHonorsOffset(t *testing.T) {
	h := newTrailerTestHandler(t, 10, 1000)
	resp, body := serve(t, h, "/nums?offset=3&limit=2")

	if got := strings.TrimSpace(body); got != `{"id":4}`+"\n"+`{"id":5}` {
		t.Fatalf("body = %q", got)
	}
	if got := resp.Trailer.Get("Content-Range"); got != "3-4/*" {
		t.Errorf("Content-Range = %q, want %q", got, "3-4/*")
	}
}

func TestEmptyResultUsesUnsatisfiedRange(t *testing.T) {
	h := newTrailerTestHandler(t, 5, 1000)
	resp, body := serve(t, h, "/nums?offset=99")

	if strings.TrimSpace(body) != "" {
		t.Fatalf("body = %q, want empty", body)
	}
	if got := resp.Trailer.Get("Content-Range"); got != "*/*" {
		t.Errorf("Content-Range = %q, want %q", got, "*/*")
	}
}

func TestTruncationTrailersSetAtRowCap(t *testing.T) {
	h := newTrailerTestHandler(t, 10, 4)
	resp, body := serve(t, h, "/nums")

	if got := len(strings.Split(strings.TrimSpace(body), "\n")); got != 4 {
		t.Fatalf("row count = %d, want 4", got)
	}
	if got := resp.Trailer.Get("X-Truncated"); got != "true" {
		t.Errorf("X-Truncated = %q, want %q", got, "true")
	}
	if got := resp.Trailer.Get("X-Max-Rows"); got != "4" {
		t.Errorf("X-Max-Rows = %q, want %q", got, "4")
	}
	if got := resp.Trailer.Get("Content-Range"); got != "0-3/*" {
		t.Errorf("Content-Range = %q, want %q", got, "0-3/*")
	}
}

// Go synthesizes a "Trailer:" announcement from the http.TrailerPrefix keys on
// a chunked response, so the names do reach the wire ahead of the body. The
// client surfaces the parsed values on resp.Trailer rather than resp.Header.
func TestTrailersAnnouncedAndDelivered(t *testing.T) {
	h := newTrailerTestHandler(t, 3, 1000)
	resp, _ := serve(t, h, "/nums")

	if got := resp.Trailer.Get("Content-Range"); got != "0-2/*" {
		t.Errorf("Content-Range = %q, want %q", got, "0-2/*")
	}
	// Untruncated responses set no truncation trailers at all.
	if got := resp.Trailer.Get("X-Truncated"); got != "" {
		t.Errorf("X-Truncated = %q, want empty", got)
	}
}
