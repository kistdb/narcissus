// Package collate finds collations other SQLite programs don't have, and
// converts a database to SQLite's built-in ones, into a new file.
package collate

import (
	"bytes"
	"maps"
	"slices"
	"strings"

	"github.com/ncruces/go-sqlite3"

	"github.com/kistdb/narcissus/internal/sqlite"
	"github.com/kistdb/narcissus/internal/sqltext"
)

// Builtin are the collations every SQLite has, as its own code.
var Builtin = []string{"binary", "nocase", "rtrim"}

// IsBuiltin reports whether name is one of SQLite's built-in collations.
func IsBuiltin(name string) bool { return slices.Contains(Builtin, sqltext.Fold(name)) }

// Use is one place a database names a collation that isn't built in.
type Use struct {
	Type      string `json:"type"` // table, index, view or trigger
	Object    string `json:"object"`
	Column    string `json:"column,omitempty"` // the column or index key, when there is one
	Collation string `json:"collation"`
}

func (u Use) String() string {
	where := u.Type + " " + u.Object
	switch {
	case strings.HasPrefix(u.Object, "sqlite_autoindex_"):
		where = "the UNIQUE or PRIMARY KEY constraint (" + u.Object + ")"
	case u.Type == "table" && u.Column != "":
		where += ", column " + u.Column
	case u.Column != "":
		where += ", key " + u.Column
	}
	return where + ": " + u.Collation
}

// Override is a NOCASE or RTRIM index whose order SQLite's own disagrees with,
// as when the app that built it replaced the collation with its own.
type Override struct {
	Index   string `json:"index"`
	Table   string `json:"table"`
	Problem string `json:"problem"` // as integrity_check words it
}

// Report is what Find learns about a database.
type Report struct {
	Uses      []Use
	Overrides []Override
	Unchecked []string // tables whose NOCASE and RTRIM indexes couldn't be checked
}

// Found reports whether anything needs attention.
func (r Report) Found() bool { return len(r.Uses) > 0 || len(r.Overrides) > 0 }

// object is a schema entry.
type object struct {
	Type, Name, Table, SQL string
}

func objects(c *sqlite3.Conn) ([]object, error) {
	var objs []object
	err := sqlite.Query(c, `SELECT type, name, tbl_name, sql FROM sqlite_schema
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY rowid`,
		func(st *sqlite3.Stmt) error {
			objs = append(objs, object{st.ColumnText(0), st.ColumnText(1), st.ColumnText(2), st.ColumnText(3)})
			return nil
		})
	return objs, err
}

// Find lists every use of a collation that isn't built in, and every NOCASE
// index whose order disagrees with SQLite's NOCASE. It only reads.
func Find(c *sqlite3.Conn) (Report, error) {
	var r Report
	objs, err := objects(c)
	if err != nil {
		return r, err
	}
	custom := make(map[string]bool) // tables touched by a custom collation
	for _, o := range objs {
		for _, u := range explicitUses(o) {
			r.Uses = append(r.Uses, u)
			custom[sqltext.Fold(o.Table)] = true
		}
	}
	nocase := make(map[string][]string) // table: its NOCASE and RTRIM indexes
	err = sqlite.Query(c, `SELECT s.name, s.tbl_name, x.name, x.coll FROM sqlite_schema AS s, pragma_index_xinfo(s.name) AS x
		WHERE s.type = 'index' AND x.key ORDER BY s.rowid, x.seqno`,
		func(st *sqlite3.Stmt) error {
			idx, table, col, coll := st.ColumnText(0), st.ColumnText(1), st.ColumnText(2), st.ColumnText(3)
			if col == "" {
				col = "(expression)"
			}
			switch {
			case !IsBuiltin(coll):
				custom[sqltext.Fold(table)] = true
				u := Use{Type: "index", Object: idx, Column: col, Collation: coll}
				// An explicit COLLATE in the index's statement is already listed, without its key.
				r.Uses = slices.DeleteFunc(r.Uses, func(e Use) bool {
					return e.Type == "index" && e.Object == idx && e.Column == "" && sqltext.Fold(e.Collation) == sqltext.Fold(coll)
				})
				if !slices.Contains(r.Uses, u) {
					r.Uses = append(r.Uses, u)
				}
			case sqltext.Fold(coll) != "binary" && !slices.Contains(nocase[table], idx):
				nocase[table] = append(nocase[table], idx)
			}
			return nil
		})
	if err != nil {
		return r, err
	}
	for _, table := range slices.Sorted(maps.Keys(nocase)) {
		if custom[sqltext.Fold(table)] {
			r.Unchecked = append(r.Unchecked, table) // integrity_check needs every collation
			continue
		}
		seen := make(map[string]bool)
		err := sqlite.Query(c, `PRAGMA integrity_check(`+sqltext.Quote(table)+`)`, func(st *sqlite3.Stmt) error {
			msg := st.ColumnText(0)
			for _, idx := range nocase[table] {
				if strings.HasSuffix(msg, "index "+idx) && !seen[idx] { // as in "row 3 missing from index t_a"
					seen[idx] = true
					r.Overrides = append(r.Overrides, Override{Index: idx, Table: table, Problem: msg})
				}
			}
			return nil
		})
		if err != nil { // a function or collation this SQLite lacks; report the rest
			r.Unchecked = append(r.Unchecked, table)
		}
	}
	return r, nil
}

// explicitUses finds COLLATE clauses naming a collation that isn't built in.
func explicitUses(o object) []Use {
	toks, err := sqltext.Lex(o.SQL)
	if err != nil {
		return nil
	}
	var uses []Use
	for i, t := range toks {
		if !t.Is("collate") || i+1 >= len(toks) || IsBuiltin(toks[i+1].Ident()) {
			continue
		}
		uses = append(uses, Use{Type: o.Type, Object: o.Name, Column: columnAt(o, toks, i), Collation: toks[i+1].Ident()})
	}
	return uses
}

// columnAt names the column a CREATE TABLE's COLLATE at toks[i] belongs to.
func columnAt(o object, toks []sqltext.Token, i int) string {
	if o.Type != "table" {
		return ""
	}
	open := slices.IndexFunc(toks, func(t sqltext.Token) bool { return t.Text == "(" })
	if open < 0 || i < open {
		return ""
	}
	defs, _ := sqltext.SplitList(toks[open+1:])
	for _, def := range defs {
		if len(def) == 0 || def[0].Start > toks[i].Start || def[len(def)-1].End < toks[i].End {
			continue
		}
		if !slices.ContainsFunc([]string{"constraint", "primary", "unique", "check", "foreign"}, def[0].Is) {
			return def[0].Ident() // a column definition, whose name may be a keyword such as key
		}
	}
	return ""
}

// Placeholders gives c a stand-in for each collation it lacks, so tables whose
// keys use them can be scanned. Use c only for NOT INDEXED scans after this.
func Placeholders(c *sqlite3.Conn, r Report) error {
	done := make(map[string]bool)
	for _, u := range r.Uses {
		if done[sqltext.Fold(u.Collation)] {
			continue
		}
		done[sqltext.Fold(u.Collation)] = true
		if err := c.CreateCollation(u.Collation, bytes.Compare); err != nil {
			return err
		}
	}
	return nil
}

// Begin starts the read transaction that MakePlan and Write share, so both see
// one snapshot, finds the collations, and gives c Placeholders for them.
func Begin(c *sqlite3.Conn) (Report, error) {
	if err := c.Exec(`BEGIN`); err != nil {
		return Report{}, err
	}
	r, err := Find(c)
	if err == nil {
		err = Placeholders(c, r)
	}
	return r, err
}
