package pocketcaddy

import (
	"fmt"
	"strings"
)

// This file implements the parser for PostgREST-style filter specs, the value
// half of a `?column=<spec>` query-string pair. The grammar is:
//
//	spec  := [ "not" "." ] op "." value | bareValue
//	op    := "eq" | "neq" | "gt" | ... | "is" | "in"
//	value := literal | "(" list ")"
//	list  := item { "," item }
//	item  := quoted | unquoted
//
// A dot is only a separator at the top level: inside a parenthesized list or a
// quoted item it is ordinary text, which is why this is a scanner rather than
// a strings.Split.

// filterScanner walks a filter spec one token at a time.
type filterScanner struct {
	src string
	pos int
}

// atEnd reports whether the whole spec has been consumed.
func (s *filterScanner) atEnd() bool { return s.pos >= len(s.src) }

// rest returns the unconsumed remainder.
func (s *filterScanner) rest() string { return s.src[s.pos:] }

// nextSegment consumes up to the next top-level '.', returning the segment and
// whether a separator was found. Parenthesized and quoted runs are skipped, so
// a dot inside `in.(1,2)` or `"a.b"` does not split the spec.
func (s *filterScanner) nextSegment() (string, bool) {
	start := s.pos
	depth := 0
	for s.pos < len(s.src) {
		switch c := s.src[s.pos]; c {
		case '"':
			s.skipQuotedItem()
			continue
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case '.':
			if depth == 0 {
				seg := s.src[start:s.pos]
				s.pos++ // consume the '.'
				return seg, true
			}
		}
		s.pos++
	}
	return s.src[start:s.pos], false
}

// skipQuotedItem advances past a double-quoted run, honoring doubled quotes.
func (s *filterScanner) skipQuotedItem() {
	s.pos++ // opening quote
	for s.pos < len(s.src) {
		if s.src[s.pos] == '"' {
			if s.pos+1 < len(s.src) && s.src[s.pos+1] == '"' {
				s.pos += 2
				continue
			}
			s.pos++
			return
		}
		s.pos++
	}
}

// parseList parses a parenthesized, comma-separated list such as `(1,2,"a,b")`.
// Quoted items keep their commas and are returned unquoted.
func parseList(src string) ([]string, error) {
	src = strings.TrimSpace(src)
	if !strings.HasPrefix(src, "(") {
		return nil, fmt.Errorf("expected '(' to open the value list")
	}
	if !strings.HasSuffix(src, ")") {
		return nil, fmt.Errorf("expected ')' to close the value list")
	}
	inner := src[1 : len(src)-1]
	if strings.TrimSpace(inner) == "" {
		return nil, fmt.Errorf("requires at least one value")
	}

	var (
		out  []string
		cur  strings.Builder
		i    int
		seen bool // an item is present, even if empty (e.g. `("")`)
	)
	for i < len(inner) {
		switch c := inner[i]; c {
		case '"':
			seen = true
			i++
			for i < len(inner) {
				if inner[i] == '"' {
					if i+1 < len(inner) && inner[i+1] == '"' {
						cur.WriteByte('"')
						i += 2
						continue
					}
					i++
					break
				}
				cur.WriteByte(inner[i])
				i++
			}
		case ',':
			out = append(out, cur.String())
			cur.Reset()
			seen = false
			i++
		default:
			seen = true
			cur.WriteByte(c)
			i++
		}
	}
	if seen || cur.Len() > 0 || len(out) > 0 {
		out = append(out, cur.String())
	}
	return out, nil
}
