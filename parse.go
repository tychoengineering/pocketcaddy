package pocketcaddy

import (
	"bufio"
	"fmt"
	"strings"
)

// Directive prefixes recognized in leading SQL line comments.
const (
	dirDB       = "@db"
	dirParam    = "@param"
	dirName     = "@name"
	dirDesc     = "@description"
	dirSingular = "@singular"
)

// ParamType is the declared type of a query parameter, used to convert
// incoming string/JSON values into the right Go type before binding.
type ParamType string

const (
	TypeText  ParamType = "text"
	TypeInt   ParamType = "int"
	TypeFloat ParamType = "float"
	TypeBool  ParamType = "bool"

	// Array types. A list parameter is written in SQL as a single placeholder
	// inside parentheses -- `WHERE id IN (:ids)` -- and expanded to one
	// placeholder per element at request time.
	TypeTextList  ParamType = "text[]"
	TypeIntList   ParamType = "int[]"
	TypeFloatList ParamType = "float[]"
)

// IsList reports whether t is an array type.
func (t ParamType) IsList() bool {
	switch t {
	case TypeTextList, TypeIntList, TypeFloatList:
		return true
	}
	return false
}

// Elem returns the element type of an array type.
func (t ParamType) Elem() ParamType {
	switch t {
	case TypeTextList:
		return TypeText
	case TypeIntList:
		return TypeInt
	case TypeFloatList:
		return TypeFloat
	}
	return t
}

// Param is a single @param declaration.
//
//	-- @param id int required   The post to fetch
//	-- @param limit int = 20
type Param struct {
	Name     string
	Type     ParamType
	Required bool
	Default  any
	HasDeflt bool
	Desc     string
}

// Query is one named SQL query parsed from a .sql file.
type Query struct {
	Name     string
	DB       string
	SQL      string
	Params   []Param
	Desc     string
	Singular bool // return a bare object instead of an array

	// Columns is the query's output column set, captured at startup. Filter,
	// order and select terms are validated against it: wrapping a query in a
	// subselect makes SQLite treat an unknown identifier as a string literal
	// rather than an error, which would silently return zero rows.
	Columns map[string]bool

	byName map[string]Param
}

// HasColumn reports whether the query's result set includes the named column.
func (q *Query) HasColumn(name string) bool {
	return q.Columns[name]
}

// Param looks up a declared parameter by name.
func (q *Query) Param(name string) (Param, bool) {
	p, ok := q.byName[name]
	return p, ok
}

// parseSQLFile parses the contents of a .sql file into a Query. The query name
// defaults to defaultName (the file's base name) unless overridden by @name.
//
// Directives are line comments (`-- @foo ...`) appearing anywhere in the file;
// everything that is not a directive comment forms the SQL body.
func parseSQLFile(defaultName, src string) (*Query, error) {
	q := &Query{
		Name:   defaultName,
		byName: map[string]Param{},
	}

	var body strings.Builder
	sc := bufio.NewScanner(strings.NewReader(src))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for lineNo := 1; sc.Scan(); lineNo++ {
		line := sc.Text()
		directive, rest, ok := splitDirective(line)
		if !ok {
			body.WriteString(line)
			body.WriteByte('\n')
			continue
		}

		switch directive {
		case dirName:
			if rest == "" {
				return nil, fmt.Errorf("line %d: @name requires a value", lineNo)
			}
			q.Name = rest
		case dirDB:
			if rest == "" {
				return nil, fmt.Errorf("line %d: @db requires a value", lineNo)
			}
			q.DB = rest
		case dirDesc:
			q.Desc = rest
		case dirSingular:
			q.Singular = true
		case dirParam:
			p, err := parseParam(rest)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			if _, dup := q.byName[p.Name]; dup {
				return nil, fmt.Errorf("line %d: duplicate @param %q", lineNo, p.Name)
			}
			q.Params = append(q.Params, p)
			q.byName[p.Name] = p
		default:
			return nil, fmt.Errorf("line %d: unknown directive %q", lineNo, directive)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	q.SQL = strings.TrimSpace(body.String())
	if q.SQL == "" {
		return nil, fmt.Errorf("no SQL statement found")
	}
	if q.DB == "" {
		return nil, fmt.Errorf("missing @db directive")
	}
	return q, nil
}

// splitDirective reports whether line is a `-- @directive rest` comment and,
// if so, returns the directive token and the remainder.
func splitDirective(line string) (directive, rest string, ok bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "--") {
		return "", "", false
	}
	t = strings.TrimSpace(strings.TrimPrefix(t, "--"))
	if !strings.HasPrefix(t, "@") {
		return "", "", false
	}
	directive, rest, _ = strings.Cut(t, " ")
	return directive, strings.TrimSpace(rest), true
}

// parseParam parses the body of an @param directive:
//
//	name [type] [required | = default] [description...]
func parseParam(s string) (Param, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return Param{}, fmt.Errorf("@param requires a name")
	}

	p := Param{Name: fields[0], Type: TypeText}
	if !validIdent(p.Name) {
		return Param{}, fmt.Errorf("invalid @param name %q", p.Name)
	}
	rest := fields[1:]

	if len(rest) > 0 {
		if t, ok := parseType(rest[0]); ok {
			p.Type = t
			rest = rest[1:]
		}
	}

	if len(rest) > 0 {
		switch {
		case rest[0] == "required":
			p.Required = true
			rest = rest[1:]
		case rest[0] == "=":
			if len(rest) < 2 {
				return Param{}, fmt.Errorf("@param %s: '=' requires a default value", p.Name)
			}
			v, err := coerceDefault(rest[1], p.Type)
			if err != nil {
				return Param{}, fmt.Errorf("@param %s default: %w", p.Name, err)
			}
			p.Default, p.HasDeflt = v, true
			rest = rest[2:]
		case strings.HasPrefix(rest[0], "="):
			v, err := coerceDefault(strings.TrimPrefix(rest[0], "="), p.Type)
			if err != nil {
				return Param{}, fmt.Errorf("@param %s default: %w", p.Name, err)
			}
			p.Default, p.HasDeflt = v, true
			rest = rest[1:]
		}
	}

	p.Desc = strings.Join(rest, " ")
	return p, nil
}

func parseType(s string) (ParamType, bool) {
	switch strings.ToLower(s) {
	case "text", "string", "str":
		return TypeText, true
	case "int", "integer":
		return TypeInt, true
	case "float", "real", "number":
		return TypeFloat, true
	case "bool", "boolean":
		return TypeBool, true
	case "text[]", "string[]", "str[]":
		return TypeTextList, true
	case "int[]", "integer[]":
		return TypeIntList, true
	case "float[]", "real[]", "number[]":
		return TypeFloatList, true
	}
	return "", false
}

func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
