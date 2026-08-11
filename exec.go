package pocketcaddy

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	sqlparser "github.com/rqlite/sql"
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
			return "", nil, errUnknownParam(name, q.Name, q.paramNames())
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
			return "", nil, errMissingParam(p.Name)
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
				return "", nil, badRequest(CodeInvalidType,
					"parameter %q expects a list", p.Name).withParam(p.Name)
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
//
// The rewrite runs over the parsed AST rather than the SQL text: a *sql.BindExpr
// node is a placeholder by construction, so a colon inside a string literal,
// quoted identifier or comment can never be mistaken for one. Bind nodes are
// visited in source order, which is the order SQLite assigns to `?`, so args is
// built by appending as each node is replaced.
func expandLists(q *Query, resolved map[string]any) (string, []any, error) {
	stmt, err := parseSQL(q.SQL)
	if err != nil {
		return "", nil, err
	}

	v := &bindExpander{q: q, resolved: resolved}
	out, err := sqlparser.Walk(v, stmt)
	if err != nil {
		return "", nil, err
	}
	if v.err != nil {
		return "", nil, v.err
	}
	return out.String(), v.args, nil
}

// parseSQL parses a single SQLite statement into its AST.
func parseSQL(src string) (sqlparser.Statement, error) {
	stmt, err := sqlparser.NewParser(strings.NewReader(src)).ParseStatement()
	if err != nil {
		return nil, fmt.Errorf("parsing SQL: %w", err)
	}
	return stmt, nil
}

// bindExpander replaces each named bind with positional placeholders,
// collecting the bind arguments in visit order.
type bindExpander struct {
	q        *Query
	resolved map[string]any
	args     []any
	err      error
}

func (v *bindExpander) Visit(n sqlparser.Node) (sqlparser.Visitor, sqlparser.Node, error) {
	return v, n, nil
}

func (v *bindExpander) VisitEnd(n sqlparser.Node) (sqlparser.Node, error) {
	if v.err != nil {
		return n, nil
	}

	switch node := n.(type) {
	case *sqlparser.ExprList:
		// Walk is bottom-up, so a list bind among these elements has already
		// become a nested ExprList. Both an `IN (...)` right-hand side and an
		// expanded list print their own parentheses, so splice the nested
		// elements into this list to keep exactly one pair.
		return flattenExprList(node), nil

	case *sqlparser.BindExpr:
		p, val, ok := v.lookup(node)
		if !ok {
			return n, nil
		}
		if p.Type.IsList() {
			exprs, empty := v.expandItems(p.Name, val)
			if empty {
				// `IN ()` is a syntax error in SQLite, and an empty list should
				// match nothing rather than everything. NULL is never equal to
				// anything, so `x IN (NULL)` is never true.
				return &sqlparser.NullLit{}, nil
			}
			if exprs == nil {
				return n, nil
			}
			return &sqlparser.ExprList{Exprs: exprs}, nil
		}
		v.args = append(v.args, val)
		return positional(), nil
	}
	return n, nil
}

// flattenExprList splices any directly nested ExprList into its parent, so an
// expanded list bind contributes its placeholders as siblings rather than as a
// separately parenthesized group.
func flattenExprList(list *sqlparser.ExprList) sqlparser.Node {
	out := make([]sqlparser.Expr, 0, len(list.Exprs))
	for _, e := range list.Exprs {
		if inner, ok := e.(*sqlparser.ExprList); ok {
			out = append(out, inner.Exprs...)
			continue
		}
		out = append(out, e)
	}
	return &sqlparser.ExprList{Exprs: out}
}

// expandItems turns a resolved list value into one placeholder per element,
// appending each element to args. empty reports a zero-length list; a nil
// slice with empty false means the value was not a list and v.err is set.
func (v *bindExpander) expandItems(name string, val any) (exprs []sqlparser.Expr, empty bool) {
	items, ok := val.([]any)
	if !ok {
		v.err = badRequest(CodeInvalidType, "parameter %q expects a list", name).withParam(name)
		return nil, false
	}
	if len(items) == 0 {
		return nil, true
	}
	exprs = make([]sqlparser.Expr, len(items))
	for i, item := range items {
		exprs[i] = positional()
		v.args = append(v.args, item)
	}
	return exprs, false
}

// lookup resolves a bind node to its declared parameter and bound value.
func (v *bindExpander) lookup(b *sqlparser.BindExpr) (Param, any, bool) {
	// BindExpr.Name carries the leading marker, e.g. ":ids".
	name := strings.TrimPrefix(b.Name, ":")
	p, ok := v.q.Param(name)
	if !ok {
		// The query text references a parameter it never declared. That is an
		// authoring bug in the .sql file, not bad client input, so it is a 500
		// rather than a 400 -- nothing the caller sent could have avoided it.
		v.err = newError(http.StatusInternalServerError, CodeQueryFailed,
			"query %q references undeclared parameter %q", v.q.Name, name)
		return Param{}, nil, false
	}
	return p, v.resolved[name], true
}

// positional returns a fresh `?` placeholder node.
func positional() *sqlparser.BindExpr { return &sqlparser.BindExpr{Name: "?"} }

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
