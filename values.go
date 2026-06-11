package pocketcaddy

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// coerce converts a raw string (from a query parameter) to the declared type.
func coerce(raw string, t ParamType) (any, error) {
	switch t {
	case TypeInt:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", raw)
		}
		return n, nil
	case TypeFloat:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", raw)
		}
		return f, nil
	case TypeBool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("%q is not a boolean", raw)
		}
		return b, nil
	default:
		return raw, nil
	}
}

// coerceDefault parses an @param default value. List defaults are written
// comma-separated, e.g. `-- @param ids int[] = 1,2,3`; an empty string yields
// an empty list.
func coerceDefault(raw string, t ParamType) (any, error) {
	if !t.IsList() {
		return coerce(raw, t)
	}
	raw = strings.TrimSuffix(strings.TrimPrefix(raw, "("), ")")
	if raw == "" {
		return []any{}, nil
	}
	out := []any{}
	for _, item := range strings.Split(raw, ",") {
		v, err := coerce(strings.TrimSpace(item), t.Elem())
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// coerceList converts a slice of raw strings (repeated query params) into the
// declared element type.
func coerceList(raws []string, t ParamType) (any, error) {
	out := make([]any, 0, len(raws))
	for _, raw := range raws {
		v, err := coerce(raw, t.Elem())
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// coerceJSON converts a decoded JSON value to the declared type. JSON already
// carries type information, so this mostly validates and narrows numbers.
func coerceJSON(v any, t ParamType) (any, error) {
	if v == nil {
		return nil, nil
	}

	if t.IsList() {
		items, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("expected an array, got %T", v)
		}
		out := make([]any, 0, len(items))
		for i, item := range items {
			ev, err := coerceJSON(item, t.Elem())
			if err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
			out = append(out, ev)
		}
		return out, nil
	}

	switch t {
	case TypeInt:
		switch n := v.(type) {
		case json.Number:
			i, err := n.Int64()
			if err != nil {
				return nil, fmt.Errorf("%v is not an integer", v)
			}
			return i, nil
		case float64:
			if n != float64(int64(n)) {
				return nil, fmt.Errorf("%v is not an integer", v)
			}
			return int64(n), nil
		case string:
			return coerce(n, t)
		}
		return nil, fmt.Errorf("expected integer, got %T", v)
	case TypeFloat:
		switch n := v.(type) {
		case json.Number:
			f, err := n.Float64()
			if err != nil {
				return nil, fmt.Errorf("%v is not a number", v)
			}
			return f, nil
		case float64:
			return n, nil
		case string:
			return coerce(n, t)
		}
		return nil, fmt.Errorf("expected number, got %T", v)
	case TypeBool:
		switch b := v.(type) {
		case bool:
			return b, nil
		case string:
			return coerce(b, t)
		}
		return nil, fmt.Errorf("expected boolean, got %T", v)
	default:
		switch s := v.(type) {
		case string:
			return s, nil
		case json.Number:
			return s.String(), nil
		case bool:
			return strconv.FormatBool(s), nil
		case float64:
			return strconv.FormatFloat(s, 'f', -1, 64), nil
		}
		return nil, fmt.Errorf("expected string, got %T", v)
	}
}

// --- PostgREST-style query options ------------------------------------------

// operators maps PostgREST operator tokens to SQL comparison operators.
var operators = map[string]string{
	"eq":   "=",
	"neq":  "<>",
	"gt":   ">",
	"gte":  ">=",
	"lt":   "<",
	"lte":  "<=",
	"like": "LIKE",
	"ilike": "LIKE", // SQLite LIKE is case-insensitive for ASCII by default
}

// filter is one PostgREST-style predicate applied to the result of a named
// query, e.g. `?status=eq.active` or `?score=gte.10`.
type filter struct {
	Column string
	Op     string
	Negate bool
	Value  any
	IsNull bool
	InList []any
}

// options holds the PostgREST-style shaping applied on top of a named query.
type options struct {
	Select  []string
	Filters []filter
	Order   []orderTerm
	Limit   int
	Offset  int
	HasLim  bool
	Single  bool
}

type orderTerm struct {
	Column string
	Desc   bool
	// NullsLast is only emitted when explicitly requested.
	Nulls string // "", "first", "last"
}

// parseFilter parses a `column=op.value` pair into a filter, checking the
// column against the query's output columns.
func parseFilter(column, spec string, q *Query) (filter, error) {
	f := filter{Column: column}
	if err := checkColumn(column, q); err != nil {
		return f, err
	}

	op, val, ok := strings.Cut(spec, ".")
	if !ok {
		// Bare `column=value` is shorthand for eq.
		f.Op = "="
		f.Value = spec
		return f, nil
	}

	if op == "not" {
		f.Negate = true
		op, val, ok = strings.Cut(val, ".")
		if !ok {
			return f, fmt.Errorf("filter %q: expected not.<op>.<value>", spec)
		}
	}

	switch op {
	case "is":
		switch strings.ToLower(val) {
		case "null":
			f.IsNull = true
			return f, nil
		case "true":
			f.Op, f.Value = "=", true
			return f, nil
		case "false":
			f.Op, f.Value = "=", false
			return f, nil
		}
		return f, fmt.Errorf("filter %q: is.<null|true|false> only", spec)
	case "in":
		list := strings.TrimSuffix(strings.TrimPrefix(val, "("), ")")
		if list == "" {
			return f, fmt.Errorf("filter %q: in.() requires values", spec)
		}
		for _, item := range splitCSV(list) {
			f.InList = append(f.InList, item)
		}
		f.Op = "IN"
		return f, nil
	}

	sqlOp, known := operators[op]
	if !known {
		return f, fmt.Errorf("unknown operator %q in filter for %q", op, column)
	}
	f.Op = sqlOp
	f.Value = val
	return f, nil
}

// splitCSV splits a PostgREST in-list, honoring double-quoted items.
func splitCSV(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case c == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	out = append(out, cur.String())
	return out
}

// checkColumn verifies that a client-supplied identifier is well-formed and
// present in the query's result set. Both checks matter: validIdent keeps the
// value safe to interpolate as a quoted identifier, and the column-set check
// turns a typo into a 400 rather than a silently empty result.
func checkColumn(name string, q *Query) error {
	if !validIdent(name) {
		return fmt.Errorf("invalid column name %q", name)
	}
	if q != nil && len(q.Columns) > 0 && !q.HasColumn(name) {
		return fmt.Errorf("query %q has no column %q", q.Name, name)
	}
	return nil
}

// parseOrder parses `order=col.desc,other.asc.nullslast`.
func parseOrder(spec string, q *Query) ([]orderTerm, error) {
	var terms []orderTerm
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bits := strings.Split(part, ".")
		t := orderTerm{Column: bits[0]}
		if err := checkColumn(t.Column, q); err != nil {
			return nil, err
		}
		for _, mod := range bits[1:] {
			switch strings.ToLower(mod) {
			case "asc":
				t.Desc = false
			case "desc":
				t.Desc = true
			case "nullsfirst":
				t.Nulls = "first"
			case "nullslast":
				t.Nulls = "last"
			default:
				return nil, fmt.Errorf("unknown order modifier %q", mod)
			}
		}
		terms = append(terms, t)
	}
	return terms, nil
}

// parseSelect parses `select=a,b,c` with optional `alias:column` renaming.
func parseSelect(spec string, q *Query) ([]string, error) {
	var cols []string
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		alias, col, renamed := strings.Cut(part, ":")
		if renamed {
			if !validIdent(alias) {
				return nil, fmt.Errorf("invalid select alias %q", alias)
			}
			if err := checkColumn(col, q); err != nil {
				return nil, err
			}
			cols = append(cols, quoteIdent(col)+" AS "+quoteIdent(alias))
			continue
		}
		if err := checkColumn(part, q); err != nil {
			return nil, err
		}
		cols = append(cols, quoteIdent(part))
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("select requires at least one column")
	}
	return cols, nil
}

// quoteIdent renders an identifier as a double-quoted SQL identifier. Callers
// must have already validated it with validIdent.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// buildWrapper wraps a named query as a subselect and applies the PostgREST
// shaping options, returning the final SQL and the additional bind arguments.
func (o *options) buildWrapper(inner string) (string, []any) {
	var sb strings.Builder
	var args []any

	sb.WriteString("SELECT ")
	if len(o.Select) > 0 {
		sb.WriteString(strings.Join(o.Select, ", "))
	} else {
		sb.WriteString("*")
	}
	sb.WriteString("\nFROM (\n")
	sb.WriteString(inner)
	sb.WriteString("\n) AS _q")

	if len(o.Filters) > 0 {
		sb.WriteString("\nWHERE ")
		for i, f := range o.Filters {
			if i > 0 {
				sb.WriteString(" AND ")
			}
			sb.WriteString(renderFilter(f, &args))
		}
	}

	if len(o.Order) > 0 {
		sb.WriteString("\nORDER BY ")
		for i, t := range o.Order {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(quoteIdent(t.Column))
			if t.Desc {
				sb.WriteString(" DESC")
			} else {
				sb.WriteString(" ASC")
			}
			switch t.Nulls {
			case "first":
				sb.WriteString(" NULLS FIRST")
			case "last":
				sb.WriteString(" NULLS LAST")
			}
		}
	}

	if o.HasLim {
		sb.WriteString("\nLIMIT ?")
		args = append(args, o.Limit)
	}
	if o.Offset > 0 {
		if !o.HasLim {
			// SQLite requires LIMIT before OFFSET.
			sb.WriteString("\nLIMIT -1")
		}
		sb.WriteString(" OFFSET ?")
		args = append(args, o.Offset)
	}

	return sb.String(), args
}

func renderFilter(f filter, args *[]any) string {
	col := quoteIdent(f.Column)

	switch {
	case f.IsNull:
		if f.Negate {
			return col + " IS NOT NULL"
		}
		return col + " IS NULL"
	case f.Op == "IN":
		ph := make([]string, len(f.InList))
		for i, v := range f.InList {
			ph[i] = "?"
			*args = append(*args, v)
		}
		expr := col + " IN (" + strings.Join(ph, ", ") + ")"
		if f.Negate {
			return "NOT (" + expr + ")"
		}
		return expr
	default:
		*args = append(*args, f.Value)
		expr := col + " " + f.Op + " ?"
		if f.Negate {
			return "NOT (" + expr + ")"
		}
		return expr
	}
}
