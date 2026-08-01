package pocketcaddy

import (
	"strings"
	"testing"
)

// mkQuery builds a Query with the given params for binder tests.
func mkQuery(sql string, params ...Param) *Query {
	q := &Query{Name: "t", DB: "app", SQL: sql, byName: map[string]Param{}}
	for _, p := range params {
		q.Params = append(q.Params, p)
		q.byName[p.Name] = p
	}
	return q
}

func TestBindArgsScalarUsesNamedParams(t *testing.T) {
	q := mkQuery("SELECT * FROM t WHERE a = :a", Param{Name: "a", Type: TypeInt})
	sql, args, err := bindArgs(q, map[string]any{"a": int64(3)})
	if err != nil {
		t.Fatal(err)
	}
	if sql != q.SQL {
		t.Errorf("scalar-only SQL should be untouched, got %q", sql)
	}
	if len(args) != 1 {
		t.Fatalf("want 1 arg, got %d", len(args))
	}
}

func TestBindArgsExpandsList(t *testing.T) {
	q := mkQuery("SELECT * FROM t WHERE id IN (:ids)",
		Param{Name: "ids", Type: TypeIntList, Required: true})

	sql, args, err := bindArgs(q, map[string]any{"ids": []any{int64(1), int64(2), int64(3)}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "IN (?, ?, ?)") {
		t.Errorf("want three placeholders, got %q", sql)
	}
	if len(args) != 3 {
		t.Fatalf("want 3 args, got %d: %v", len(args), args)
	}
}

func TestBindArgsEmptyListMatchesNothing(t *testing.T) {
	q := mkQuery("SELECT * FROM t WHERE id IN (:ids)",
		Param{Name: "ids", Type: TypeIntList})

	sql, args, err := bindArgs(q, map[string]any{"ids": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 0 {
		t.Errorf("empty list should bind no args, got %v", args)
	}
	if !strings.Contains(sql, "WHERE 0") {
		t.Errorf("empty list should yield a never-true subselect, got %q", sql)
	}
}

func TestBindArgsMixedListAndScalar(t *testing.T) {
	q := mkQuery("SELECT * FROM t WHERE id IN (:ids) AND status = :status",
		Param{Name: "ids", Type: TypeIntList, Required: true},
		Param{Name: "status", Type: TypeText, Default: "active", HasDeflt: true},
	)
	sql, args, err := bindArgs(q, map[string]any{"ids": []any{int64(7), int64(8)}})
	if err != nil {
		t.Fatal(err)
	}
	// Once a list is present everything binds positionally, in SQL order.
	if strings.Contains(sql, ":") {
		t.Errorf("expected no named placeholders left, got %q", sql)
	}
	if len(args) != 3 {
		t.Fatalf("want 3 args (2 ids + status), got %d: %v", len(args), args)
	}
	if args[2] != "active" {
		t.Errorf("scalar default should bind after the list, got %v", args[2])
	}
}

func TestBindArgsMissingRequired(t *testing.T) {
	q := mkQuery("SELECT :a", Param{Name: "a", Type: TypeInt, Required: true})
	if _, _, err := bindArgs(q, nil); err == nil {
		t.Fatal("expected an error for a missing required param")
	}
}

func TestBindArgsRejectsUnknownParam(t *testing.T) {
	q := mkQuery("SELECT 1")
	if _, _, err := bindArgs(q, map[string]any{"nope": 1}); err == nil {
		t.Fatal("expected an error for an undeclared param")
	}
}

func TestBindArgsRejectsScalarForListParam(t *testing.T) {
	q := mkQuery("SELECT * FROM t WHERE id IN (:ids)",
		Param{Name: "ids", Type: TypeIntList})
	if _, _, err := bindArgs(q, map[string]any{"ids": int64(5)}); err == nil {
		t.Fatal("expected an error when a scalar is given for a list param")
	}
}

// walkPlaceholders must not treat a colon inside a literal, identifier or
// comment as a parameter -- doing so would corrupt the SQL.
func TestWalkPlaceholdersIgnoresNonParameterColons(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string // parameter names expected to be seen
	}{
		{"string literal", `SELECT 'a:b' , :real`, []string{"real"}},
		{"escaped quote", `SELECT 'it''s :not' , :real`, []string{"real"}},
		{"double-quoted ident", `SELECT "col:x" FROM t WHERE a = :real`, []string{"real"}},
		{"backtick ident", "SELECT `c:x` FROM t WHERE a = :real", []string{"real"}},
		{"line comment", "-- :nope\nSELECT :real", []string{"real"}},
		{"block comment", "/* :nope */ SELECT :real", []string{"real"}},
		{"cast operator", `SELECT x::text, :real`, []string{"real"}},
		{"time literal", `SELECT '12:30:00', :real`, []string{"real"}},
		{"multiple", `SELECT :a, :b WHERE c = ':skip'`, []string{"a", "b"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			var sb strings.Builder
			err := walkPlaceholders(tc.sql, &sb, func(name string) (string, error) {
				seen = append(seen, name)
				return "?", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(seen, ",") != strings.Join(tc.want, ",") {
				t.Errorf("saw params %v, want %v (sql=%q)", seen, tc.want, tc.sql)
			}
		})
	}
}

func TestWalkPlaceholdersPreservesText(t *testing.T) {
	var sb strings.Builder
	err := walkPlaceholders(`SELECT 'a:b' FROM t WHERE id = :id`, &sb,
		func(string) (string, error) { return "?", nil })
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT 'a:b' FROM t WHERE id = ?`
	if sb.String() != want {
		t.Errorf("got %q, want %q", sb.String(), want)
	}
}

func TestParseParamListTypesAndDefaults(t *testing.T) {
	p, err := parseParam("ids int[] = 1,2,3   The ids")
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != TypeIntList {
		t.Errorf("type = %v, want int[]", p.Type)
	}
	items, ok := p.Default.([]any)
	if !ok || len(items) != 3 {
		t.Fatalf("default = %#v, want a 3-element list", p.Default)
	}
	if items[0] != int64(1) {
		t.Errorf("elements should be coerced to int64, got %T", items[0])
	}
	if p.Desc != "The ids" {
		t.Errorf("desc = %q", p.Desc)
	}
}

func TestParseParamRejectsBadListDefault(t *testing.T) {
	if _, err := parseParam("ids int[] = 1,abc"); err == nil {
		t.Fatal("expected an error for a non-integer element")
	}
}

func TestCoerceJSONList(t *testing.T) {
	v, err := coerceJSON([]any{float64(1), float64(2)}, TypeIntList)
	if err != nil {
		t.Fatal(err)
	}
	items := v.([]any)
	if len(items) != 2 || items[1] != int64(2) {
		t.Errorf("got %#v", items)
	}

	if _, err := coerceJSON("notalist", TypeIntList); err == nil {
		t.Fatal("expected an error when a scalar is given for a list type")
	}
	if _, err := coerceJSON([]any{float64(1.5)}, TypeIntList); err == nil {
		t.Fatal("expected an error for a fractional int element")
	}
}
