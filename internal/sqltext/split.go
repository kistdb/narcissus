package sqltext

import (
	"errors"
	"strings"
)

// Statement is one statement of SQL text.
type Statement struct {
	Tokens     []Token
	Start, End int      // byte offsets of the first token and the end of the last
	Line       int      // line of the first token, from 1
	Comments   []string // "--" comments on the lines directly above, without the dashes
}

// Split splits the named file's src into statements at semicolons. A
// semicolon inside a trigger body splits it too; sqlite.Script.Run rejoins those.
func Split(file, src string) ([]Statement, error) {
	toks, err := Lex(src)
	if serr, ok := errors.AsType[*Error](err); ok {
		serr.File = file
	}
	if err != nil {
		return nil, err
	}
	var stmts []Statement
	prevEnd, start := 0, 0
	line, lineOff := 1, 0
	add := func(toks []Token) {
		first := toks[0].Start
		line += strings.Count(src[lineOff:first], "\n")
		lineOff = first
		stmts = append(stmts, Statement{
			Tokens:   toks,
			Start:    first,
			End:      toks[len(toks)-1].End,
			Line:     line,
			Comments: leadComments(src[prevEnd:first], prevEnd > 0),
		})
	}
	for i, t := range toks {
		if t.Text != ";" {
			continue
		}
		if i > start {
			add(toks[start:i])
		}
		prevEnd, start = t.End, i+1
	}
	if start < len(toks) {
		add(toks[start:])
	}
	return stmts, nil
}

// leadComments returns the "--" comment lines that end gap, the text before
// a statement. A blank or other line ends the run; midLine skips gap's first line.
func leadComments(gap string, midLine bool) []string {
	lines := strings.Split(gap, "\n")
	lines = lines[:len(lines)-1] // the statement's own line
	if midLine && len(lines) > 0 {
		lines = lines[1:]
	}
	var comments []string
	for _, l := range lines {
		l = strings.TrimSpace(strings.TrimPrefix(l, bom))
		switch {
		case strings.HasPrefix(l, "--"):
			comments = append(comments, strings.TrimSpace(l[2:]))
		default:
			comments = nil
		}
	}
	return comments
}

// SQL returns the statement as written, without comments and with each run
// of white space reduced to one space.
func (s Statement) SQL() string {
	var b strings.Builder
	for i, t := range s.Tokens {
		if i > 0 && t.Start > s.Tokens[i-1].End {
			b.WriteByte(' ')
		}
		b.WriteString(t.Text)
	}
	return b.String()
}

// SplitList splits the tokens after an opening parenthesis at their top-level
// commas, returning the terms and the position of the closing parenthesis.
func SplitList(toks []Token) (terms [][]Token, end int) {
	depth, start := 0, 0
	for i, t := range toks {
		switch {
		case t.Text == "(":
			depth++
		case t.Text == ")" && depth > 0:
			depth--
		case t.Text == ")" || t.Text == "," && depth == 0:
			terms = append(terms, toks[start:i])
			start = i + 1
			if t.Text == ")" {
				return terms, i
			}
		}
	}
	return append(terms, toks[start:]), len(toks)
}
