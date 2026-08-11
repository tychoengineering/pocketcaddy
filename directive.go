package pocketcaddy

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file implements the tokenizer for `-- @directive` comment lines and the
// recursive-descent parser for an @param declaration. Both work over a token
// stream with source positions rather than over raw strings, so an error can
// point at the offending column and a quoted value keeps its spaces.

// tokenKind classifies a token in a directive line.
type tokenKind int

const (
	tokEOF tokenKind = iota
	tokWord
	tokString // a quoted value, already unquoted
	tokEq     // '='
	tokComma  // ','
	tokLParen // '('
	tokRParen // ')'
)

func (k tokenKind) String() string {
	switch k {
	case tokEOF:
		return "end of line"
	case tokWord:
		return "word"
	case tokString:
		return "quoted string"
	case tokEq:
		return "'='"
	case tokComma:
		return "','"
	case tokLParen:
		return "'('"
	case tokRParen:
		return "')'"
	}
	return "token"
}

// token is a single lexeme with its column, 1-based, within the directive body.
type token struct {
	Kind tokenKind
	Text string
	Col  int
}

// lexer produces tokens from the body of a directive line.
type lexer struct {
	src string
	pos int
}

func newLexer(src string) *lexer { return &lexer{src: src} }

// rest returns the unconsumed remainder, trimmed. Used for trailing free-form
// text such as a parameter description, which is not tokenized.
func (l *lexer) rest() string { return strings.TrimSpace(l.src[l.pos:]) }

func (l *lexer) skipSpace() {
	for l.pos < len(l.src) {
		r, w := utf8.DecodeRuneInString(l.src[l.pos:])
		if !unicode.IsSpace(r) {
			return
		}
		l.pos += w
	}
}

// next returns the next token, consuming it.
func (l *lexer) next() (token, error) {
	l.skipSpace()
	if l.pos >= len(l.src) {
		return token{Kind: tokEOF, Col: l.pos + 1}, nil
	}

	start := l.pos
	c := l.src[l.pos]

	switch c {
	case '=':
		l.pos++
		return token{Kind: tokEq, Text: "=", Col: start + 1}, nil
	case ',':
		l.pos++
		return token{Kind: tokComma, Text: ",", Col: start + 1}, nil
	case '(':
		l.pos++
		return token{Kind: tokLParen, Text: "(", Col: start + 1}, nil
	case ')':
		l.pos++
		return token{Kind: tokRParen, Text: ")", Col: start + 1}, nil
	case '"', '\'':
		return l.lexString()
	}

	// A bare word runs to the next space or structural character. This keeps
	// types like `int[]` intact while still splitting `ids=1`.
	for l.pos < len(l.src) {
		r, w := utf8.DecodeRuneInString(l.src[l.pos:])
		if unicode.IsSpace(r) || strings.ContainsRune("=,()\"'", r) {
			break
		}
		l.pos += w
	}
	return token{Kind: tokWord, Text: l.src[start:l.pos], Col: start + 1}, nil
}

// lexString scans a quoted value, honoring doubled quotes as an escape.
func (l *lexer) lexString() (token, error) {
	quote := l.src[l.pos]
	start := l.pos
	l.pos++

	var sb strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == quote {
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == quote {
				sb.WriteByte(quote)
				l.pos += 2
				continue
			}
			l.pos++
			return token{Kind: tokString, Text: sb.String(), Col: start + 1}, nil
		}
		sb.WriteByte(c)
		l.pos++
	}
	return token{}, fmt.Errorf("column %d: unterminated %c-quoted value", start+1, quote)
}

// peek returns the next token without consuming it.
func (l *lexer) peek() (token, error) {
	save := l.pos
	t, err := l.next()
	l.pos = save
	return t, err
}
