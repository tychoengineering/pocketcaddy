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
//	POST /<query>   {"params": {...}}   (or a bare object of params)
//	GET  /          -> catalog of available queries
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	name := strings.Trim(r.URL.Path, "/")
	// Caddy strips the matched prefix via handle_path; a placeholder may also
	// carry the remainder.
	if name == "" {
		if r.Method != http.MethodGet {
			return h.fail(w, http.StatusMethodNotAllowed, "method not allowed")
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
		return h.fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}

	vals, opts, err := h.collectInput(r, q)
	if err != nil {
		return h.fail(w, http.StatusBadRequest, err.Error())
	}

	sqlText, args, err := bindArgs(q, vals)
	if err != nil {
		return h.fail(w, http.StatusBadRequest, err.Error())
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
			h.log.Error("query failed mid-stream",
				zap.String("query", q.Name),
				zap.Int64("rows_written", res.Rows),
				zap.Error(err),
			)
			_ = writeJSONLine(w, map[string]any{
				"_error": "query failed after partial response",
				"_rows":  res.Rows,
			})
			flush()
			return nil
		}
		h.log.Error("query failed", zap.String("query", q.Name), zap.Error(err))
		return h.fail(w, http.StatusInternalServerError, "query execution failed")
	}

	// Trailing metadata line: JSONL has no envelope, so summary info rides
	// along as a final record distinguishable by its underscore-prefixed keys.
	meta := map[string]any{
		"_meta":       true,
		"_rows":       res.Rows,
		"_elapsed_ms": time.Since(start).Milliseconds(),
	}
	if res.Truncated {
		meta["_truncated"] = true
		meta["_max_rows"] = h.MaxRows
	}
	_ = writeJSONLine(w, meta)
	flush()

	h.log.Debug("query served",
		zap.String("query", q.Name),
		zap.Int64("rows", res.Rows),
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
				return nil, nil, fmt.Errorf("parameter %q: %w", key, err)
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
				return nil, nil, fmt.Errorf("limit must be a non-negative integer")
			}
			opts.Limit, opts.HasLim = n, true
		case "offset":
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				return nil, nil, fmt.Errorf("offset must be a non-negative integer")
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

// decodeBody reads the JSON POST body. Both a bare object of parameters and a
// {"params": {...}} envelope are accepted.
func (h *Handler) decodeBody(r *http.Request, q *Query, vals map[string]any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt := strings.TrimSpace(strings.Split(ct, ";")[0])
		if mt != "application/json" && mt != "" {
			return fmt.Errorf("unsupported Content-Type %q, expected application/json", mt)
		}
	}

	body := http.MaxBytesReader(nil, r.Body, h.MaxBodyBytes)
	raw, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("reading request body: %w", err)
	}
	if len(raw) == 0 {
		return nil
	}

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()

	var envelope map[string]json.RawMessage
	if err := dec.Decode(&envelope); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}

	fields := envelope
	if p, ok := envelope["params"]; ok && len(envelope) == 1 {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(p, &inner); err != nil {
			return fmt.Errorf(`invalid "params" object: %w`, err)
		}
		fields = inner
	}

	for name, rawVal := range fields {
		p, ok := q.Param(name)
		if !ok {
			return fmt.Errorf("unknown parameter %q for query %q", name, q.Name)
		}
		var anyVal any
		d := json.NewDecoder(strings.NewReader(string(rawVal)))
		d.UseNumber()
		if err := d.Decode(&anyVal); err != nil {
			return fmt.Errorf("parameter %q: %w", name, err)
		}
		v, err := coerceJSON(anyVal, p.Type)
		if err != nil {
			return fmt.Errorf("parameter %q: %w", name, err)
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

// fail writes a single-line JSONL error record. It is only safe before any row
// has been written.
func (h *Handler) fail(w http.ResponseWriter, status int, msg string) error {
	w.Header().Set("Content-Type", contentTypeJSONL)
	w.WriteHeader(status)
	return writeJSONLine(w, map[string]any{"_error": msg, "_status": status})
}
