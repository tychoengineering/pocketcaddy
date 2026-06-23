package pocketcaddy

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// bindArgs resolves the declared parameters of q against the supplied values,
// returning the SQL to execute and its bind arguments. Values not declared as
// @param are rejected so typos surface loudly rather than silently binding NULL.
//
// Scalar parameters bind by name. List parameters cannot: SQLite binds one
// value per placeholder, so `IN (:ids)` is rewritten to `IN (?, ?, ?)` with the
// element values appended positionally. Mixing named and positional binds in
// one statement is not portable, so once any list is present every parameter is
// bound positionally.
func bindArgs(q *Query, vals map[string]any) (string, []any, error) {
	for name := range vals {
		if _, ok := q.Param(name); !ok {
			return "", nil, fmt.Errorf("unknown parameter %q for query %q", name, q.Name)
		}
	}

	resolved := make(map[string]any, len(q.Params))
	hasList := false
	for _, p := range q.Params {
		v, provided := vals[p.Name]
		switch {
		case provided:
			// use as-is
		case p.HasDeflt:
			v = p.Default
		case p.Required:
			return "", nil, fmt.Errorf("missing required parameter %q", p.Name)
		default:
			if p.Type.IsList() {
				v = []any{}
			} else {
				v = nil
			}
		}
		if p.Type.IsList() {
			hasList = true
			if _, ok := v.([]any); !ok {
				return "", nil, fmt.Errorf("parameter %q expects a list", p.Name)
			}
		}
		resolved[p.Name] = v
	}

	if !hasList {
		args := make([]any, 0, len(q.Params))
		for _, p := range q.Params {
			args = append(args, sql.Named(p.Name, resolved[p.Name]))
		}
		return q.SQL, args, nil
	}

	return expandLists(q, resolved)
}

// expandLists rewrites every `:name` reference in the SQL to positional
// placeholders, expanding list parameters to one placeholder per element.
func expandLists(q *Query, resolved map[string]any) (string, []any, error) {
	var sb strings.Builder
	var args []any

	err := walkPlaceholders(q.SQL, &sb, func(name string) (string, error) {
		p, ok := q.Param(name)
		if !ok {
			return "", fmt.Errorf("SQL references undeclared parameter %q", name)
		}
		v := resolved[name]

		if !p.Type.IsList() {
			args = append(args, v)
			return "?", nil
		}

		items := v.([]any)
		if len(items) == 0 {
			// `IN ()` is a syntax error in SQLite, and an empty list should
			// match nothing rather than everything. SELECT NULL yields a
			// single NULL row, and `x IN (NULL)` is never true.
			return "SELECT NULL WHERE 0", nil
		}
		ph := make([]string, len(items))
		for i, item := range items {
			ph[i] = "?"
			args = append(args, item)
		}
		return strings.Join(ph, ", "), nil
	})
	if err != nil {
		return "", nil, err
	}
	return sb.String(), args, nil
}

// walkPlaceholders scans SQL for `:name` placeholders, writing the text to out
// and substituting each placeholder with the result of replace. String
// literals, quoted identifiers and comments are copied verbatim so a colon
// inside them is never mistaken for a parameter.
func walkPlaceholders(src string, out *strings.Builder, replace func(name string) (string, error)) error {
	for i := 0; i < len(src); {
		c := src[i]

		switch {
		case c == '\'', c == '"', c == '`':
			j := skipQuoted(src, i)
			out.WriteString(src[i:j])
			i = j
			continue

		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			j := i
			for j < len(src) && src[j] != '\n' {
				j++
			}
			out.WriteString(src[i:j])
			i = j
			continue

		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			j := i + 2
			for j+1 < len(src) && !(src[j] == '*' && src[j+1] == '/') {
				j++
			}
			if j+1 < len(src) {
				j += 2
			} else {
				j = len(src)
			}
			out.WriteString(src[i:j])
			i = j
			continue

		case c == ':':
			// `::` is a cast operator, not a placeholder.
			if i+1 < len(src) && src[i+1] == ':' {
				out.WriteString("::")
				i += 2
				continue
			}
			j := i + 1
			for j < len(src) && isIdentByte(src[j], j == i+1) {
				j++
			}
			if j == i+1 {
				out.WriteByte(c)
				i++
				continue
			}
			rep, err := replace(src[i+1 : j])
			if err != nil {
				return err
			}
			out.WriteString(rep)
			i = j
			continue
		}

		out.WriteByte(c)
		i++
	}
	return nil
}

// skipQuoted returns the index just past the quoted run beginning at i,
// honoring doubled-quote escapes.
func skipQuoted(src string, i int) int {
	q := src[i]
	j := i + 1
	for j < len(src) {
		if src[j] == q {
			if j+1 < len(src) && src[j+1] == q {
				j += 2
				continue
			}
			return j + 1
		}
		j++
	}
	return len(src)
}

func isIdentByte(c byte, first bool) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		return true
	case !first && c >= '0' && c <= '9':
		return true
	}
	return false
}

// streamResult reports what happened during a streamed query. It is only
// complete once streamQuery returns.
type streamResult struct {
	Rows      int64
	Truncated bool
}

// errAfterFirstRow marks an error that occurred after bytes were already
// flushed to the client. The status line is long gone at that point, so the
// handler can only log it and drop the connection rather than write a 500.
type errAfterFirstRow struct{ err error }

func (e errAfterFirstRow) Error() string { return e.err.Error() }
func (e errAfterFirstRow) Unwrap() error { return e.err }

// streamQuery executes the query and writes each row to w as a single line of
// JSON, flushing as it goes. Rows are never accumulated: memory stays flat at
// one row plus the write buffer regardless of result-set size.
//
// The caller must not have written a response body yet, since streamQuery may
// report a pre-flush failure that still deserves an error status.
func streamQuery(
	ctx context.Context,
	db *sql.DB,
	query string,
	args []any,
	w io.Writer,
	flush func(),
	maxRows int64,
) (streamResult, error) {
	var res streamResult

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return res, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return res, err
	}

	bw := bufio.NewWriterSize(w, 32*1024)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)

	// Reused across rows; json.Encoder writes it out fully before we refill it.
	scan := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range scan {
		ptrs[i] = &scan[i]
	}
	rec := make(map[string]any, len(cols))

	for rows.Next() {
		if maxRows > 0 && res.Rows >= maxRows {
			res.Truncated = true
			break
		}
		if err := ctx.Err(); err != nil {
			return res, wrapAfterFirst(res.Rows, err)
		}
		if err := rows.Scan(ptrs...); err != nil {
			return res, wrapAfterFirst(res.Rows, err)
		}
		for i, c := range cols {
			rec[c] = normalize(scan[i])
		}
		// Encode writes the object plus a trailing newline: exactly JSONL.
		if err := enc.Encode(rec); err != nil {
			return res, wrapAfterFirst(res.Rows, err)
		}
		res.Rows++

		// Flush periodically so slow queries stream rather than buffer, while
		// still amortizing syscalls across a batch of small rows.
		if res.Rows%256 == 0 {
			if err := bw.Flush(); err != nil {
				return res, wrapAfterFirst(res.Rows, err)
			}
			if flush != nil {
				flush()
			}
		}
	}
	if err := rows.Err(); err != nil {
		return res, wrapAfterFirst(res.Rows, err)
	}
	if err := bw.Flush(); err != nil {
		return res, wrapAfterFirst(res.Rows, err)
	}
	if flush != nil {
		flush()
	}
	return res, nil
}

// wrapAfterFirst tags errors that happened once rows were already on the wire.
func wrapAfterFirst(written int64, err error) error {
	if written > 0 {
		return errAfterFirstRow{err}
	}
	return err
}

// writeJSONLine appends a single JSON object as one JSONL line. Used for the
// trailing metadata record and for streamed error records.
func writeJSONLine(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// normalize converts driver values into types that marshal cleanly to JSON.
func normalize(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		// SQLite returns both TEXT and BLOB as []byte. Valid UTF-8 is emitted
		// as a JSON string; anything else would be mangled, so it is left as
		// []byte for encoding/json to base64-encode.
		if utf8.Valid(x) {
			return string(x)
		}
		return x
	case time.Time:
		return x.Format(time.RFC3339Nano)
	default:
		return v
	}
}
