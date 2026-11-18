package sqltext

import (
	"reflect"
	"strings"
	"testing"
)

func TestLex(t *testing.T) {
	src := `SELECT "a""b", [c d], ` + "`e`" + `, 'it''s', x'00', 1.5e-3, ?1, :id, a->>'$.x' -- note
	/* block */ FROM t WHERE a<>b;`
	toks, err := Lex(src)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tok := range toks {
		got = append(got, tok.Canon())
	}
	want := []string{"select", `"a""b"`, ",", `"c d"`, ",", "e", ",", "'it''s'", ",", "x'00'", ",", "1.5e-3", ",",
		"?1", ",", ":id", ",", "a", "->>", "'$.x'", "from", "t", "where", "a", "<>", "b", ";"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Lex() =\n%q\nwant\n%q", got, want)
	}
}

func TestLexErrors(t *testing.T) {
	for _, src := range []string{"select 'x", `select "x`, "select [x", "select 1\n/* x"} {
		if _, err := Lex(src); err == nil {
			t.Errorf("Lex(%q) succeeded, want an error", src)
		}
	}
	_, err := Split("q.sql", "select 1;\n\nselect 'x")
	if err == nil || !strings.HasPrefix(err.Error(), "q.sql:3: ") {
		t.Errorf("error %v, want it at q.sql:3", err)
	}
}

func TestSplit(t *testing.T) {
	src := `-- file header

-- name: one
-- narcissus: allow full-scan
SELECT 1;  -- trailing note
-- name: two
SELECT ';'
  ,   2 ;

/* block */ SELECT 3; SELECT 4
`
	stmts, err := Split("q.sql", src)
	if err != nil {
		t.Fatal(err)
	}
	type got struct {
		SQL      string
		Line     int
		Comments []string
	}
	var gots []got
	for _, s := range stmts {
		gots = append(gots, got{s.SQL(), s.Line, s.Comments})
	}
	want := []got{
		{"SELECT 1", 5, []string{"name: one", "narcissus: allow full-scan"}},
		{"SELECT ';' , 2", 7, []string{"name: two"}},
		{"SELECT 3", 10, nil},
		{"SELECT 4", 10, nil},
	}
	if !reflect.DeepEqual(gots, want) {
		t.Errorf("Split() =\n%+v\nwant\n%+v", gots, want)
	}
}

func TestSkipSpace(t *testing.T) {
	tests := map[string]int{
		"":                0,
		"  x":             2,
		"-- a\n/* b */ x": 13,
		"-- only":         7,
		"/* open":         0,
		"\n\t select":     3,
		"\uFEFFselect":    3,
	}
	for s, want := range tests {
		if got := SkipSpace(s); got != want {
			t.Errorf("SkipSpace(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestCanon(t *testing.T) {
	canon := func(src string) string {
		toks, err := Lex(src)
		if err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, tok := range toks {
			parts = append(parts, tok.Canon())
		}
		return strings.Join(parts, " ")
	}
	same := [][2]string{
		{`Email DESC`, `"email" desc`},
		{`[Order]`, `"order"`},
		{"`x`", `X`},
	}
	for _, p := range same {
		if canon(p[0]) != canon(p[1]) {
			t.Errorf("Canon(%q) = %q, Canon(%q) = %q; want equal", p[0], canon(p[0]), p[1], canon(p[1]))
		}
	}
	differ := [][2]string{
		{`a desc`, `"a desc"`},
		{`"É"`, `"é"`},
		{`'A'`, `'a'`},
	}
	for _, p := range differ {
		if canon(p[0]) == canon(p[1]) {
			t.Errorf("Canon(%q) == Canon(%q) = %q; want different", p[0], p[1], canon(p[0]))
		}
	}
}

func TestFold(t *testing.T) {
	if got := Fold("ÉcoLE_Ä"); got != "École_Ä" {
		t.Errorf("Fold() = %q, want ASCII letters only lowered", got)
	}
}
