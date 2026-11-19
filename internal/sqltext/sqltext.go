// Package sqltext splits and normalises SQLite SQL without a database.
package sqltext

import (
	"errors"
	"fmt"
	"strings"
)

// Kind is the kind of a token.
type Kind int

// The token kinds.
const (
	Word   Kind = iota // keyword or bare identifier
	Quoted             // "x", `x` or [x]
	String             // 'x' or x'00'
	Number
	Param
	Punct
)

// Token is one token of SQL text.
type Token struct {
	Kind       Kind
	Text       string
	Start, End int // byte offsets in the source
}

// Ident returns the token's text as a name, without quotes.
func (t Token) Ident() string {
	switch {
	case t.Kind != Quoted && t.Kind != String:
		return t.Text
	case t.Text[0] == '[':
		return t.Text[1 : len(t.Text)-1]
	default:
		q := t.Text[:1]
		return strings.ReplaceAll(t.Text[1:len(t.Text)-1], q+q, q)
	}
}

// Canon returns the token as compared: bare and bracketed names case-folded,
// double-quoted text as written, since it may be a string literal.
func (t Token) Canon() string {
	switch {
	case t.Kind == Word:
		return Fold(t.Text)
	case t.Kind == Quoted && t.Text[0] != '"':
		return Quote(Fold(t.Ident()))
	case t.Kind == Quoted:
		return `"` + strings.ReplaceAll(t.Ident(), `"`, `""`) + `"`
	default:
		return t.Text
	}
}

// Is reports whether the token is the keyword or bare identifier word.
func (t Token) Is(word string) bool {
	return t.Kind == Word && Fold(t.Text) == Fold(word) // ASCII only, as SQLite
}

// Error is an error at a line of a SQL file.
type Error struct {
	File string
	Line int
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("%s:%d: %v", e.File, e.Line, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

// ErrUnterminatedComment is a /* comment that runs to the end of the text.
var ErrUnterminatedComment = errors.New("unterminated comment")

// Lex splits src into tokens, dropping white space and comments.
func Lex(src string) ([]Token, error) {
	if i := strings.IndexByte(src, 0); i >= 0 {
		return nil, &Error{Line: LineAt(src, i), Err: errors.New("NUL byte in SQL")}
	}
	var toks []Token
	for i := SkipSpace(src); i < len(src); i += SkipSpace(src[i:]) {
		end, kind, err := scan(src, i)
		if err != nil {
			return nil, &Error{Line: LineAt(src, i), Err: err}
		}
		toks = append(toks, Token{Kind: kind, Text: src[i:end], Start: i, End: end})
		i = end
	}
	return toks, nil
}

// SkipSpace returns the length of the white space and comments that start s.
func SkipSpace(s string) int {
	i := 0
	for i < len(s) {
		switch {
		case isSpace(s[i]):
			i++
		case strings.HasPrefix(s[i:], bom):
			i += len(bom)
		case strings.HasPrefix(s[i:], "--"):
			n := strings.IndexByte(s[i:], '\n')
			if n < 0 {
				return len(s)
			}
			i += n + 1
		case strings.HasPrefix(s[i:], "/*"):
			n := strings.Index(s[i+2:], "*/")
			if n < 0 {
				return i // unterminated: left for the caller to report
			}
			i += n + 4
		default:
			return i
		}
	}
	return i
}

const bom = "\uFEFF" // SQLite reads a byte-order mark as white space

// LineAt returns the 1-based line of byte offset i in s.
func LineAt(s string, i int) int {
	return strings.Count(s[:i], "\n") + 1
}

var operators = []string{"->>", "->", "<>", "!=", "<=", ">=", "==", "||", "<<", ">>"}

// scan returns the end and kind of the token at s[i].
func scan(s string, i int) (int, Kind, error) {
	c := s[i]
	switch {
	case strings.HasPrefix(s[i:], "/*"):
		return 0, 0, ErrUnterminatedComment
	case c == '\'':
		end, err := quoted(s, i, '\'')
		return end, String, err
	case (c == 'x' || c == 'X') && i+1 < len(s) && s[i+1] == '\'':
		end, err := quoted(s, i+1, '\'')
		return end, String, err
	case c == '"' || c == '`':
		end, err := quoted(s, i, c)
		return end, Quoted, err
	case c == '[':
		n := strings.IndexByte(s[i:], ']')
		if n < 0 {
			return 0, 0, errors.New("unterminated identifier")
		}
		return i + n + 1, Quoted, nil
	case isDigit(c) || c == '.' && i+1 < len(s) && isDigit(s[i+1]):
		return number(s, i), Number, nil
	case c == '?' || c == ':' || c == '@' || c == '$':
		j := i + 1
		for j < len(s) && isIdent(s[j]) {
			j++
		}
		return j, Param, nil
	case isIdent(c):
		j := i + 1
		for j < len(s) && isIdent(s[j]) {
			j++
		}
		return j, Word, nil
	}
	for _, op := range operators {
		if strings.HasPrefix(s[i:], op) {
			return i + len(op), Punct, nil
		}
	}
	return i + 1, Punct, nil
}

// number returns the end of the numeric literal at s[i], scanned as SQLite
// does: a sign belongs to it only after a decimal exponent marker and before a digit.
func number(s string, i int) int {
	digits := func(j int, ok func(byte) bool) int {
		for j < len(s) && (ok(s[j]) || s[j] == '_') {
			j++
		}
		return j
	}
	if strings.HasPrefix(s[i:], "0x") || strings.HasPrefix(s[i:], "0X") {
		i = digits(i+2, isHex)
	} else {
		i = digits(i, isDigit)
		if i < len(s) && s[i] == '.' {
			i = digits(i+1, isDigit)
		}
		if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
			j := i + 1
			if j < len(s) && (s[j] == '+' || s[j] == '-') {
				j++
			}
			if j < len(s) && isDigit(s[j]) {
				i = digits(j, isDigit)
			}
		}
	}
	for i < len(s) && isIdent(s[i]) { // SQLite rejects such a token; keep it whole
		i++
	}
	return i
}

func isHex(c byte) bool { return isDigit(c) || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F' }

// quoted returns the end of the quoted text at s[i], where a doubled quote
// is an escaped one.
func quoted(s string, i int, q byte) (int, error) {
	for j := i + 1; j < len(s); j++ {
		if s[j] != q {
			continue
		}
		if j+1 < len(s) && s[j+1] == q {
			j++
			continue
		}
		return j + 1, nil
	}
	return 0, fmt.Errorf("unterminated %c", q)
}

func isSpace(c byte) bool { return c == ' ' || '\t' <= c && c <= '\r' } // as SQLite: \t \n \v \f \r
func isDigit(c byte) bool { return '0' <= c && c <= '9' }

func isIdent(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || isDigit(c) || c == '_' || c == '$' || c >= 0x80
}
