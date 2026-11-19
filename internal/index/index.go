// Package index parses CREATE INDEX statements and plans the changes that
// bring a database's indexes in line with them.
package index

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kistdb/narcissus/internal/sqltext"
)

// Index is an index created with CREATE INDEX.
type Index struct {
	Name   string
	Table  string
	Unique bool
	SQL    string // the statement, compacted to one line
	File   string // its file; empty when read from a database
	Line   int
	// AllowUnused is set by a "-- narcissus: allow unused" comment above it.
	AllowUnused bool
	canon       string
}

// Equal reports whether two indexes have the same definition, ignoring
// layout, comments, identifier quoting and keyword case.
func (ix Index) Equal(other Index) bool { return ix.canon == other.canon }

func (ix Index) String() string { return ix.SQL }

func (ix Index) key() string { return sqltext.Fold(ix.Name) }

// Raw returns an index SQLite stores but Parse can't read; it equals only an identical statement.
func Raw(name, table, sql string) Index {
	return Index{Name: name, Table: table, SQL: sql, canon: "raw " + sql}
}

// Parse parses the named file's src, which must hold only CREATE INDEX
// statements, each naming a different index.
func Parse(file, src string) ([]Index, error) {
	stmts, err := sqltext.Split(file, src)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]int)
	var ixs []Index
	for _, st := range stmts {
		ix, err := parse(st)
		if err == nil {
			ix.AllowUnused, err = allowUnused(st.Comments)
		}
		if err != nil {
			return nil, &sqltext.Error{File: file, Line: st.Line, Err: err}
		}
		ix.File = file
		if line, ok := seen[ix.key()]; ok {
			return nil, &sqltext.Error{File: file, Line: st.Line, Err: fmt.Errorf("index %s already declared on line %d", ix.Name, line)}
		}
		seen[ix.key()] = st.Line
		ixs = append(ixs, ix)
	}
	return ixs, nil
}

// FromSQL parses one CREATE INDEX statement, as SQLite stores it.
func FromSQL(sql string) (Index, error) {
	stmts, err := sqltext.Split("", sql)
	if err != nil {
		return Index{}, err
	}
	if len(stmts) != 1 {
		return Index{}, fmt.Errorf("want one CREATE INDEX statement, got %d statements", len(stmts))
	}
	ix, err := parse(stmts[0])
	ix.Line = 0
	return ix, err
}

var errNotIndex = errors.New("want only CREATE INDEX statements")

// parse reads CREATE [UNIQUE] INDEX [IF NOT EXISTS] [main.]name ON table (...).
func parse(st sqltext.Statement) (Index, error) {
	toks := st.Tokens
	i := 0
	next := func() (sqltext.Token, bool) {
		if i >= len(toks) {
			return sqltext.Token{}, false
		}
		i++
		return toks[i-1], true
	}
	accept := func(word string) bool {
		if i < len(toks) && toks[i].Is(word) {
			i++
			return true
		}
		return false
	}
	ident := func() (sqltext.Token, bool) {
		t, ok := next()
		return t, ok && (t.Kind == sqltext.Word || t.Kind == sqltext.Quoted || t.Kind == sqltext.String)
	}

	if !accept("create") {
		return Index{}, errNotIndex
	}
	ix := Index{Unique: accept("unique"), SQL: st.SQL(), Line: st.Line}
	if !accept("index") {
		return Index{}, errNotIndex
	}
	if accept("if") && (!accept("not") || !accept("exists")) {
		return Index{}, errors.New("want IF NOT EXISTS")
	}
	name, ok := ident()
	if !ok {
		return Index{}, errors.New("want an index name")
	}
	if i < len(toks) && toks[i].Text == "." {
		if sqltext.Fold(name.Ident()) != "main" {
			return Index{}, fmt.Errorf("indexes in schema %q are not supported", name.Ident())
		}
		i++
		if name, ok = ident(); !ok {
			return Index{}, errors.New("want an index name")
		}
	}
	if !accept("on") {
		return Index{}, errors.New("want ON after the index name")
	}
	table, ok := ident()
	if !ok || i >= len(toks) || toks[i].Text != "(" {
		return Index{}, errors.New("want a table name and column list after ON")
	}
	ix.Name, ix.Table = name.Ident(), table.Ident()

	canon := []string{"create", "index", sqltext.Quote(sqltext.Fold(ix.Name)), "on", sqltext.Quote(sqltext.Fold(ix.Table))}
	if ix.Unique {
		canon[0] = "create unique"
	}
	for _, t := range toks[i:] {
		canon = append(canon, t.Canon())
	}
	ix.canon = strings.Join(canon, " ")
	return ix, nil
}

// allowUnused reads the "-- narcissus:" comments above an index.
func allowUnused(comments []string) (bool, error) {
	allow := false
	for _, c := range comments {
		key, value, ok := strings.Cut(c, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "narcissus") {
			continue
		}
		if !strings.EqualFold(strings.Join(strings.Fields(value), " "), "allow unused") {
			return false, fmt.Errorf("unknown directive %q (want allow unused)", strings.TrimSpace(value))
		}
		allow = true
	}
	return allow, nil
}

// Renamed returns the statement creating the index under another name.
func (ix Index) Renamed(name string) (string, error) {
	toks, err := sqltext.Lex(ix.SQL)
	if err != nil {
		return "", err
	}
	i := 1 // after CREATE
	for _, w := range []string{"unique", "index", "if", "not", "exists"} {
		if i < len(toks) && toks[i].Is(w) {
			i++
		}
	}
	if i+1 < len(toks) && toks[i+1].Text == "." {
		i += 2 // main.
	}
	if i >= len(toks) {
		return "", fmt.Errorf("index %s: can't find its name", ix.Name)
	}
	return ix.SQL[:toks[i].Start] + sqltext.Quote(name) + ix.SQL[toks[i].End:], nil
}
