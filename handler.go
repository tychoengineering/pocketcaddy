package pocketcaddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// contentTypeJSONL is the media type used for streamed result sets.
const contentTypeJSONL = "application/x-ndjson"

// ServeHTTP routes GET/POST requests to named queries.
//
//	GET  /<query>?col=eq.value&order=col.desc&limit=10
//	POST /<query>   {"param": value, ...}
//	GET  /          -> catalog of available queries
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	name := strings.Trim(r.URL.Path, "/")
	// Caddy strips the matched prefix via handle_path; a placeholder may also
	// carry the remainder.
	if name == "" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			return h.fail(w, newError(http.StatusMethodNotAllowed, CodeMethodNotAllowed,
				"the catalog supports GET only, not %s", r.Method))
		}
		return h.writeCatalog(w)
	}

	q, ok := h.store.queries[name]
	if !ok {
		// Not one of ours: let the rest of the Caddy chain try.
		return next.ServeHTTP(w, r)
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost:
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		return h.fail(w, newError(http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			"query %q supports GET, HEAD and POST, not %s", q.Name, r.Method))
	}

	vals, opts, err := h.collectInput(r, q)
	if err != nil {
		return h.fail(w, err)
	}

	sqlText, args, err := bindArgs(q, vals)
	if err != nil {
		return h.fail(w, err)
	}

	if opts.needsWrapper() {
		wrapped, extra := opts.buildWrapper(stripTrailingSemicolon(sqlText))
		sqlText = wrapped
		args = append(args, extra...)
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(h.TimeoutSec)*time.Second)
	defer cancel()

	w.Header().Set("Content-Type", contentTypeJSONL)
	w.Header().Set("X-Query-Name", q.Name)
	// The body length is unknown until the last row, so never let a proxy or
	// the client expect otherwise.
	w.Header().Set("Cache-Control", "no-store")
	// Row count and truncation are only known once the last row is written, by
	// which point the header block is long gone, so both ride as trailers set
	// via http.TrailerPrefix. That is the only mechanism that works after the
	// response is committed; it also means the names cannot be announced in a
	// "Trailer" header, since Go emits announced trailers only for values set
	// before the first write.

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return nil
	}

	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	start := time.Now()
	res, err := streamQuery(ctx, h.store.dbs[q.DB], sqlText, args, w, flush, h.MaxRows)
	if err != nil {
		var partial errAfterFirstRow
		if errors.As(err, &partial) {
			// Rows are already on the wire; the status is committed. Emit a
			// trailing error record so a JSONL consumer can detect the break,
			// then log it.
			h.failMidStream(w, q.Name, res.Rows, partial.Unwrap())
			setRangeTrailer(w, opts.Offset, res.Rows)
			flush()
			return nil
		}
		// A cancelled context is a timeout, not a query fault; asError sorts the
		// two, so only a genuine driver failure becomes a 500.
		if errors.Is(err, context.DeadlineExceeded) {
			return h.fail(w, errTimeout(err))
		}
		return h.fail(w, errQueryFailed(err))
	}

	setRangeTrailer(w, opts.Offset, res.Rows)
	if res.Truncated {
		w.Header().Set(http.TrailerPrefix+"X-Truncated", "true")
		w.Header().Set(http.TrailerPrefix+"X-Max-Rows", strconv.FormatInt(h.MaxRows, 10))
	}
	flush()

	h.log.Debug("query served",
		zap.String("query", q.Name),
		zap.Int64("rows", res.Rows),
		zap.Bool("truncated", res.Truncated),
		zap.Duration("elapsed", time.Since(start)),
	)
	return nil
}

// collectInput gathers bind values and PostgREST options from the request.
// POST takes params from the JSON body; both methods honor query-string
// options (select/order/limit/offset) and filters.
func (h *Handler) collectInput(r *http.Request, q *Query) (map[string]any, *options, error) {
	vals := map[string]any{}
	opts := &options{}

	if r.Method == http.MethodPost {
		if err := h.decodeBody(r, q, vals); err != nil {
			return nil, nil, err
		}
	}

	query := r.URL.Query()
	for key, list := range query {
		if len(list) == 0 {
			continue
		}
		raw := list[0]

		// Declared @param wins over filter interpretation, so a query with a
		// `status` param is not shadowed by a `status=eq.x` filter.
		if p, ok := q.Param(key); ok {
			if _, already := vals[key]; already {
				continue // body value takes precedence
			}
			var (
				v   any
				err error
			)
			if p.Type.IsList() {
				// A list param accepts repetition (?id=1&id=2) and, for a
				// single occurrence, a comma-separated value (?id=1,2).
				items := list
				if len(items) == 1 {
					items = strings.Split(items[0], ",")
				}
				v, err = coerceList(items, p.Type)
			} else {
				v, err = coerce(raw, p.Type)
			}
			if err != nil {
				return nil, nil, errInvalidType(key, err)
			}
			vals[key] = v
			continue
		}

		switch key {
		case "select":
			cols, err := parseSelect(raw, q)
			if err != nil {
				return nil, nil, err
			}
			opts.Select = cols
		case "order":
			terms, err := parseOrder(raw, q)
			if err != nil {
				return nil, nil, err
			}
			opts.Order = terms
		case "limit":
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				return nil, nil, badRequest(CodeInvalidRange,
					"limit must be a non-negative integer, got %q", raw).withParam("limit")
			}
			opts.Limit, opts.HasLim = n, true
		case "offset":
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				return nil, nil, badRequest(CodeInvalidRange,
					"offset must be a non-negative integer, got %q", raw).withParam("offset")
			}
			opts.Offset = n
		default:
			// Every remaining key is a PostgREST filter. Repeated keys stack
			// as AND, matching PostgREST semantics.
			for _, spec := range list {
				f, err := parseFilter(key, spec, q)
				if err != nil {
					return nil, nil, err
				}
				opts.Filters = append(opts.Filters, f)
			}
		}
	}

	return vals, opts, nil
}

// decodeBody reads the JSON POST body, a bare object whose keys are the
// query's declared parameters.
func (h *Handler) decodeBody(r *http.Request, q *Query, vals map[string]any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt := strings.TrimSpace(strings.Split(ct, ";")[0])
		if mt != "application/json" && mt != "" {
			return newError(http.StatusUnsupportedMediaType, CodeUnsupportedMedia,
				"unsupported Content-Type %q, expected application/json", mt)
		}
	}

	body := http.MaxBytesReader(nil, r.Body, h.MaxBodyBytes)
	raw, err := io.ReadAll(body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			e := newError(http.StatusRequestEntityTooLarge, CodeBodyTooLarge,
				"request body exceeds the %d byte limit", h.MaxBodyBytes)
			e.Meta = map[string]any{"limit": h.MaxBodyBytes}
			return e
		}
		return badRequest(CodeInvalidBody, "reading request body: %v", err)
	}
	if len(raw) == 0 {
		return nil
	}

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()

	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil {
		return badRequest(CodeInvalidBody, "body is not a JSON object: %v", err)
	}

	for name, rawVal := range fields {
		p, ok := q.Param(name)
		if !ok {
			return errUnknownParam(name, q.Name, q.paramNames()).withPointer("/" + name)
		}
		var anyVal any
		d := json.NewDecoder(strings.NewReader(string(rawVal)))
		d.UseNumber()
		if err := d.Decode(&anyVal); err != nil {
			return badRequest(CodeInvalidBody, "parameter %q: %v", name, err).
				withPointer("/" + name)
		}
		v, err := coerceJSON(anyVal, p.Type)
		if err != nil {
			return errInvalidType(name, err).withPointer("/" + name)
		}
		vals[name] = v
	}
	return nil
}

// needsWrapper reports whether any PostgREST shaping was requested.
func (o *options) needsWrapper() bool {
	return len(o.Select) > 0 || len(o.Filters) > 0 || len(o.Order) > 0 ||
		o.HasLim || o.Offset > 0
}

// stripTrailingSemicolon removes a trailing `;` so the SQL can be nested in a
// subselect.
func stripTrailingSemicolon(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), ";")
}

// writeCatalog lists the available queries as JSONL, one record per query.
func (h *Handler) writeCatalog(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", contentTypeJSONL)
	w.Header().Set("Cache-Control", "no-store")

	for _, name := range h.store.queryNames() {
		q := h.store.queries[name]
		params := make([]map[string]any, 0, len(q.Params))
		for _, p := range q.Params {
			rec := map[string]any{
				"name":     p.Name,
				"type":     string(p.Type),
				"required": p.Required,
			}
			if p.HasDeflt {
				rec["default"] = p.Default
			}
			if p.Desc != "" {
				rec["description"] = p.Desc
			}
			params = append(params, rec)
		}
		if err := writeJSONLine(w, map[string]any{
			"name":        q.Name,
			"db":          q.DB,
			"description": q.Desc,
			"params":      params,
		}); err != nil {
			return err
		}
	}
	return nil
}

// setRangeTrailer records how much of the result set this response covers, as
// offset-first..offset-last out of an unknown total. Computing an exact total
// would mean a second COUNT(*) per request, so the total is always "*".
//
// A zero-row response has no satisfiable range, so it uses the RFC 7233
// unsatisfied form rather than a backwards one like "5-4".
func setRangeTrailer(w http.ResponseWriter, offset int, rows int64) {
	const key = http.TrailerPrefix + "Content-Range"
	if rows == 0 {
		w.Header().Set(key, "*/*")
		return
	}
	first := int64(offset)
	w.Header().Set(key, fmt.Sprintf("%d-%d/*", first, first+rows-1))
}

// fail writes a single-line JSONL error record and the matching status. It is
// only safe before any row has been written; once the response is committed,
// use failMidStream instead.
//
// A non-*Error is treated as an internal failure: an error reaching here
// without a code is a bug, so it is logged rather than rendered verbatim.
func (h *Handler) fail(w http.ResponseWriter, err error) error {
	e := asError(err)
	if e.Status >= http.StatusInternalServerError {
		h.log.Error("request failed",
			zap.String("code", string(e.Code)),
			zap.Error(err),
		)
	}
	w.Header().Set("Content-Type", contentTypeJSONL)
	w.WriteHeader(e.Status)
	return writeJSONLine(w, e.wire())
}

// failMidStream reports a failure that happened after rows were already
// flushed. The status line is long gone, so the error rides as a final JSONL
// record carrying the same shape as fail's, with meta marking the response
// partial. The status field is omitted: the client already received a 200.
func (h *Handler) failMidStream(w http.ResponseWriter, queryName string, rows int64, err error) {
	e := asError(err)
	h.log.Error("query failed mid-stream",
		zap.String("query", queryName),
		zap.String("code", string(e.Code)),
		zap.Int64("rows_written", rows),
		zap.Error(err),
	)

	partial := &Error{Code: e.Code, Detail: e.Detail, Source: e.Source}
	partial.Meta = map[string]any{"partial": true, "rows": rows}
	for k, v := range e.Meta {
		partial.Meta[k] = v
	}
	_ = writeJSONLine(w, partial.wire())
}

// asError coerces any error into an *Error so a single render path serves every
// failure. An unclassified error becomes a generic 500 rather than leaking an
// arbitrary Go message as a code-bearing detail.
func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errTimeout(err)
	}
	return &Error{
		Code:   CodeQueryFailed,
		Status: http.StatusInternalServerError,
		Detail: "internal error",
		err:    err,
	}
}
