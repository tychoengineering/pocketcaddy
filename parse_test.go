package pocketcaddy

import "testing"

// A quoted default keeps its spaces and commas, which strings.Fields could not.
func TestParseParamQuotedDefault(t *testing.T) {
	p, err := parseParam(`greeting text = "hello, world"   The greeting`)
	if err != nil {
		t.Fatal(err)
	}
	if p.Default != "hello, world" {
		t.Errorf("default = %#v, want %q", p.Default, "hello, world")
	}
	if p.Desc != "The greeting" {
		t.Errorf("desc = %q", p.Desc)
	}
}

func TestParseParamListDefaults(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []any
		desc string
	}{
		{"bare", `ids int[] = 1,2,3   The ids`, []any{int64(1), int64(2), int64(3)}, "The ids"},
		{"spaced", `ids int[] = 1, 2, 3`, []any{int64(1), int64(2), int64(3)}, ""},
		{"parenthesized", `ids int[] = (1, 2, 3)  Ids`, []any{int64(1), int64(2), int64(3)}, "Ids"},
		{"quoted items", `tags text[] = "a,b",c`, []any{"a,b", "c"}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parseParam(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := p.Default.([]any)
			if !ok {
				t.Fatalf("default = %#v, want a list", p.Default)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("element %d = %#v, want %#v", i, got[i], tc.want[i])
				}
			}
			if p.Desc != tc.desc {
				t.Errorf("desc = %q, want %q", p.Desc, tc.desc)
			}
		})
	}
}

func TestParseParamErrors(t *testing.T) {
	cases := []struct{ name, src string }{
		{"no name", ``},
		{"bad name", `1bad int`},
		{"dangling equals", `x int =`},
		{"unterminated quote", `x text = "abc`},
		{"bad list element", `ids int[] = 1,abc`},
		{"unclosed list paren", `ids int[] = (1,2`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseParam(tc.src); err == nil {
				t.Fatalf("expected an error for %q", tc.src)
			}
		})
	}
}

// A description must not be mistaken for a default value, and vice versa.
func TestParseParamRequiredAndDesc(t *testing.T) {
	p, err := parseParam(`id int required   The post id`)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Required {
		t.Error("want Required")
	}
	if p.HasDeflt {
		t.Error("required param should have no default")
	}
	if p.Desc != "The post id" {
		t.Errorf("desc = %q", p.Desc)
	}
}

func TestSplitDirective(t *testing.T) {
	cases := []struct {
		line      string
		directive string
		rest      string
		ok        bool
	}{
		{`-- @name post`, "@name", "post", true},
		{`  --   @db   app  `, "@db", "app", true},
		{`-- @singular`, "@singular", "", true},
		{`-- just a comment`, "", "", false},
		{`SELECT 1`, "", "", false},
	}
	for _, tc := range cases {
		d, rest, ok := splitDirective(tc.line)
		if ok != tc.ok || d != tc.directive || rest != tc.rest {
			t.Errorf("splitDirective(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.line, d, rest, ok, tc.directive, tc.rest, tc.ok)
		}
	}
}

func TestParseFilterInList(t *testing.T) {
	f, err := parseFilter("status", `in.("a,b",c)`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Op != "IN" {
		t.Errorf("op = %q, want IN", f.Op)
	}
	if len(f.InList) != 2 || f.InList[0] != "a,b" || f.InList[1] != "c" {
		t.Errorf("list = %#v, want [a,b c]", f.InList)
	}
}

// A dot inside a parenthesized list or quoted item is data, not a separator.
func TestParseFilterDotsInsideValues(t *testing.T) {
	f, err := parseFilter("v", `in.(1.5,2.5)`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.InList) != 2 || f.InList[0] != "1.5" || f.InList[1] != "2.5" {
		t.Errorf("list = %#v, want [1.5 2.5]", f.InList)
	}

	f, err = parseFilter("name", `eq.a.b`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Value != "a.b" {
		t.Errorf("value = %#v, want %q", f.Value, "a.b")
	}
}

func TestParseFilterNegated(t *testing.T) {
	f, err := parseFilter("status", `not.in.(a,b)`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Negate {
		t.Error("want Negate")
	}
	if f.Op != "IN" || len(f.InList) != 2 {
		t.Errorf("got op=%q list=%#v", f.Op, f.InList)
	}
}

func TestParseFilterErrors(t *testing.T) {
	for _, spec := range []string{`in.()`, `in.`, `not.`, `is.maybe`, `bogus.1`} {
		if _, err := parseFilter("c", spec, nil); err == nil {
			t.Errorf("expected an error for %q", spec)
		}
	}
}
