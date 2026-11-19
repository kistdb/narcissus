package collate

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ncruces/go-sqlite3"

	"github.com/kistdb/narcissus/internal/sqlite"
	"github.com/kistdb/narcissus/internal/sqltext"
)

// Mapping says which built-in collation replaces each other one.
type Mapping struct {
	Default string            // for collations not named in Names; empty for none
	Names   map[string]string // case-folded collation name: built-in name
}

// ParseMapping reads -to and -map values, such as "nocase" and "LOCALIZED=nocase,UNICODE=binary".
func ParseMapping(to, pairs string) (Mapping, error) {
	m := Mapping{Default: sqltext.Fold(strings.TrimSpace(to)), Names: make(map[string]string)}
	if m.Default != "" && !IsBuiltin(m.Default) {
		return m, fmt.Errorf("-to %s isn't one of SQLite's built-in collations: binary, nocase or rtrim", to)
	}
	for p := range strings.SplitSeq(pairs, ",") {
		if strings.TrimSpace(p) == "" {
			continue
		}
		from, into, ok := strings.Cut(p, "=")
		into = sqltext.Fold(strings.TrimSpace(into))
		if !ok || strings.TrimSpace(from) == "" || !IsBuiltin(into) {
			return m, fmt.Errorf("-map %s: want NAME=binary, NAME=nocase or NAME=rtrim", p)
		}
		m.Names[sqltext.Fold(strings.TrimSpace(from))] = into
	}
	return m, nil
}

// Target returns the built-in collation that replaces name, if any.
func (m Mapping) Target(name string) (string, bool) {
	if IsBuiltin(name) {
		return sqltext.Fold(name), true
	}
	if t, ok := m.Names[sqltext.Fold(name)]; ok {
		return t, true
	}
	return m.Default, m.Default != ""
}

// Change is one use of a collation and what replaces it.
type Change struct {
	Use
	To string
}

// Collision is a group of rows a UNIQUE index or primary key would hold twice
// under its new collations.
type Collision struct {
	Index, Table string
	Rows         int
	Example      string
}

// Plan is what a conversion would do, and what stops it.
type Plan struct {
	Changes     []Change
	Collisions  []Collision
	Unmapped    []string // collations with no replacement
	Unchecked   []string // unique indexes whose collisions couldn't be checked
	Unsupported []string // virtual tables Narcissus can't recreate
	Problems    []string // anything else that stops a faithful copy
	Notes       []string // what the copy does differently, without stopping it
}

// Blocked reports whether the conversion can't go ahead.
func (p Plan) Blocked() bool {
	return len(p.Collisions)+len(p.Unmapped)+len(p.Unchecked)+len(p.Unsupported)+len(p.Problems) > 0
}

// changed returns, by folded table name, the folded columns whose collation changes.
func (p Plan) changed() map[string]map[string]bool {
	cols := make(map[string]map[string]bool)
	for _, c := range p.Changes {
		if c.Type == "table" && c.Column != "" {
			t := sqltext.Fold(c.Object)
			if cols[t] == nil {
				cols[t] = make(map[string]bool)
			}
			cols[t][sqltext.Fold(c.Column)] = true
		}
	}
	return cols
}

// supportedModules are the virtual table modules a converted database can be given.
var supportedModules = []string{"fts5", "rtree", "rtree_i32"}

// MakePlan works out a conversion of the database on c, which has Placeholders
// and is inside a read transaction, so the plan and the copy see one snapshot.
func MakePlan(c *sqlite3.Conn, r Report, m Mapping) (Plan, error) {
	var p Plan
	for _, u := range r.Uses {
		to, ok := m.Target(u.Collation)
		if !ok {
			if !slices.Contains(p.Unmapped, u.Collation) {
				p.Unmapped = append(p.Unmapped, u.Collation)
			}
			continue
		}
		p.Changes = append(p.Changes, Change{u, to})
	}
	objs, err := objects(c)
	if err != nil {
		return p, err
	}
	for _, o := range objs {
		if mod := module(o); mod != "" && !slices.Contains(supportedModules, mod) {
			p.Unsupported = append(p.Unsupported, fmt.Sprintf("%s (USING %s)", o.Name, mod))
		}
	}
	if len(p.Unmapped) > 0 {
		return p, nil
	}
	for _, check := range []func(*sqlite3.Conn, *Plan, Report, Mapping, []object) error{
		checkOverrides, checkTables, checkForeignKeys, checkSchema, checkStats,
	} {
		if err := check(c, &p, r, m, objs); err != nil {
			return p, err
		}
	}
	return p, collisions(c, &p, m)
}

// module returns a virtual table's module, folded, or "" for anything else.
func module(o object) string {
	toks, err := sqltext.Lex(o.SQL)
	if err != nil || o.Type != "table" || len(toks) < 5 || !toks[1].Is("virtual") {
		return ""
	}
	for i, t := range toks {
		if t.Is("using") && i+1 < len(toks) {
			return sqltext.Fold(toks[i+1].Text)
		}
	}
	return ""
}

// key is one column of a unique index, with the collation it will have.
type key struct{ name, coll string }

// collisions finds rows that unique indexes would hold twice once converted.
func collisions(c *sqlite3.Conn, p *Plan, m Mapping) error {
	changedCols := p.changed()
	type unique struct {
		name, table string
		partial     bool
	}
	var uniques []unique
	err := sqlite.Query(c, `SELECT l.name, s.name, l.partial FROM sqlite_schema AS s, pragma_index_list(s.name) AS l
		WHERE s.type = 'table' AND l."unique" ORDER BY s.name, l.name`,
		func(st *sqlite3.Stmt) error {
			uniques = append(uniques, unique{st.ColumnText(0), st.ColumnText(1), st.ColumnBool(2)})
			return nil
		})
	if err != nil {
		return err
	}
	named := make(map[string]bool) // indexes whose statements name a collation that changes
	for _, c := range p.Changes {
		if c.Type == "index" {
			named[c.Object] = true
		}
	}
	for _, u := range uniques {
		var keys []key
		changed, expr := named[u.name], false
		err := sqlite.Query(c, `SELECT name, coll FROM pragma_index_xinfo(`+sqltext.QuoteString(u.name)+`) WHERE key ORDER BY seqno`,
			func(st *sqlite3.Stmt) error {
				name, coll := st.ColumnText(0), st.ColumnText(1)
				to, _ := m.Target(coll)
				changed = changed || sqltext.Fold(to) != sqltext.Fold(coll)
				expr = expr || name == ""
				keys = append(keys, key{name, to})
				return nil
			})
		if err != nil {
			return err
		}
		where := ""
		if u.partial {
			where, err = partialWhere(c, u.name)
			if err == nil && (compares(where, changedCols[sqltext.Fold(u.table)]) || len(explicitUses(object{Type: "index", SQL: where})) > 0) {
				p.Problems = append(p.Problems, fmt.Sprintf("the UNIQUE index %s has a WHERE that compares a column whose collation changes, so the rows it covers could change", u.name))
				continue
			}
		}
		if !changed {
			continue
		}
		if expr || err != nil {
			p.Unchecked = append(p.Unchecked, u.name)
			continue
		}
		coll, err := collide(c, u.table, keys, where)
		if err != nil {
			p.Unchecked = append(p.Unchecked, u.name)
			continue
		}
		if coll != nil {
			coll.Index = u.name
			p.Collisions = append(p.Collisions, *coll)
		}
	}
	return nil
}

// collide counts rows that share keys under the new collations; NULLs never collide.
func collide(c *sqlite3.Conn, table string, keys []key, where string) (*Collision, error) {
	var cols, group, notNull []string
	for _, k := range keys {
		cols = append(cols, sqltext.Quote(k.name))
		group = append(group, sqltext.Quote(k.name)+" COLLATE "+k.coll)
		notNull = append(notNull, sqltext.Quote(k.name)+" IS NOT NULL")
	}
	q := `SELECT count(*), ` + strings.Join(cols, " || ' / ' || ") + ` FROM ` + sqltext.Quote(table) + ` NOT INDEXED
		WHERE ` + strings.Join(append(notNull, "("+cmp.Or(where, "1")+")"), " AND ") + ` GROUP BY ` + strings.Join(group, ", ") + ` HAVING count(*) > 1`
	var found *Collision
	err := sqlite.Query(c, q, func(st *sqlite3.Stmt) error {
		if found == nil {
			found = &Collision{Table: table, Example: st.ColumnText(1)}
		}
		found.Rows += st.ColumnInt(0)
		return nil
	})
	if err != nil {
		return nil, errors.Join(errors.New("couldn't check"), err)
	}
	return found, nil
}

// partialWhere returns the WHERE clause of a partial index.
func partialWhere(c *sqlite3.Conn, idx string) (string, error) {
	var sql string
	err := sqlite.Query(c, `SELECT sql FROM sqlite_schema WHERE type = 'index' AND name = `+sqltext.QuoteString(idx),
		func(st *sqlite3.Stmt) error { sql = st.ColumnText(0); return nil })
	if err != nil {
		return "", err
	}
	toks, err := sqltext.Lex(sql)
	if err != nil {
		return "", err
	}
	open := slices.IndexFunc(toks, func(t sqltext.Token) bool { return t.Text == "(" })
	if open < 0 {
		return "", errors.New("no column list")
	}
	_, end := sqltext.SplitList(toks[open+1:])
	if w := open + 1 + end + 1; w+1 < len(toks) && toks[w].Is("where") {
		return sql[toks[w+1].Start:toks[len(toks)-1].End], nil
	}
	return "", errors.New("no WHERE clause")
}

// Meaning describes how comparisons behave under a built-in collation.
func Meaning(coll string) string {
	switch sqltext.Fold(coll) {
	case "nocase":
		return "ignores the case of A to Z only; other letters keep their case and accents, so 'é' and 'É' differ"
	case "rtrim":
		return "compares exactly, except that trailing spaces are ignored"
	default:
		return "compares exactly: case, accents and spaces all count"
	}
}

// compares reports whether SQL text names one of the folded columns and does
// something a collation affects, such as comparing or sorting. Unreadable text counts.
func compares(sql string, cols map[string]bool) bool {
	toks, err := sqltext.Lex(sql)
	if err != nil {
		return true
	}
	named, compared := false, false
	for _, t := range toks {
		switch {
		case (t.Kind == sqltext.Word || t.Kind == sqltext.Quoted) && cols[sqltext.Fold(t.Ident())]:
			named = true
		case t.Kind == sqltext.Word && slices.Contains(sensitive, sqltext.Fold(t.Text)),
			t.Kind != sqltext.Word && t.Kind != sqltext.Quoted && slices.Contains(sensitive, t.Text):
			compared = true
		}
	}
	return named && compared
}

// sensitive are the operators, keywords and functions whose result can depend on a collation.
var sensitive = []string{"=", "==", "<", ">", "<=", ">=", "!=", "<>", "in", "between", "is", "case", "collate",
	"min", "max", "nullif", "iif", "if", "order", "group", "distinct"}

// checkOverrides refuses to copy a UNIQUE index the app ordered with its own
// NOCASE or RTRIM, since SQLite's could merge rows the app kept apart.
func checkOverrides(c *sqlite3.Conn, p *Plan, r Report, _ Mapping, _ []object) error {
	for _, o := range r.Overrides {
		unique := false
		err := sqlite.Query(c, `SELECT "unique" FROM pragma_index_list(`+sqltext.QuoteString(o.Table)+`) WHERE name = `+sqltext.QuoteString(o.Index),
			func(st *sqlite3.Stmt) error { unique = st.ColumnBool(0); return nil })
		if err != nil {
			return err
		}
		if unique {
			p.Problems = append(p.Problems, fmt.Sprintf("the UNIQUE index %s was built with the app's own version of its collation, which SQLite's could disagree with about which rows are equal", o.Index))
		} else {
			p.Notes = append(p.Notes, fmt.Sprintf("index %s is rebuilt with SQLite's own version of its collation, which orders some text differently from the app's", o.Index))
		}
	}
	return nil
}

// checkTables refuses generated columns and CHECK constraints that read a
// column whose collation changes, and tables whose rowid can't be named.
func checkTables(c *sqlite3.Conn, p *Plan, _ Report, _ Mapping, objs []object) error {
	changed := p.changed()
	for _, o := range objs {
		if o.Type != "table" || module(o) != "" {
			continue
		}
		toks, err := sqltext.Lex(o.SQL)
		if err != nil {
			return err
		}
		cols := changed[sqltext.Fold(o.Name)]
		open := slices.IndexFunc(toks, func(t sqltext.Token) bool { return t.Text == "(" })
		if open < 0 {
			continue
		}
		defs, _ := sqltext.SplitList(toks[open+1:])
		for _, def := range defs {
			for i, t := range def {
				generated := t.Is("as") && i+1 < len(def) && def[i+1].Text == "("
				if (generated || t.Is("check")) && compares(o.SQL[def[i].Start:def[len(def)-1].End], cols) {
					what := "a CHECK constraint"
					if generated {
						what = "generated column " + def[0].Ident()
					}
					p.Problems = append(p.Problems, fmt.Sprintf("in table %s, %s compares a column whose collation changes, so its result could change", o.Name, what))
					break
				}
			}
		}
		if err := checkRowid(c, p, o.Name); err != nil {
			return err
		}
	}
	return nil
}

func checkRowid(c *sqlite3.Conn, p *Plan, table string) error {
	withoutRowid := false
	names := make(map[string]bool)
	err := sqlite.Query(c, `SELECT wr FROM pragma_table_list WHERE schema = 'main' AND name = `+sqltext.QuoteString(table),
		func(st *sqlite3.Stmt) error { withoutRowid = st.ColumnBool(0); return nil })
	if err == nil {
		err = sqlite.Query(c, `SELECT name FROM pragma_table_xinfo(`+sqltext.QuoteString(table)+`)`,
			func(st *sqlite3.Stmt) error { names[sqltext.Fold(st.ColumnText(0))] = true; return nil })
	}
	if err == nil && !withoutRowid && names["rowid"] && names["_rowid_"] && names["oid"] {
		p.Problems = append(p.Problems, fmt.Sprintf("table %s has columns named rowid, _rowid_ and oid, so its rowids can't be copied", table))
	}
	return err
}

// checkForeignKeys refuses when a child row would lose its parent row because
// the parent key's collation changes.
func checkForeignKeys(c *sqlite3.Conn, p *Plan, _ Report, m Mapping, objs []object) error {
	for _, o := range objs {
		if o.Type != "table" || module(o) != "" {
			continue
		}
		type fk struct {
			parent   string
			from, to []string
		}
		fks := make(map[int]*fk)
		err := sqlite.Query(c, `SELECT id, "table", "from", "to" FROM pragma_foreign_key_list(`+sqltext.QuoteString(o.Name)+`) ORDER BY id, seq`,
			func(st *sqlite3.Stmt) error {
				f := fks[st.ColumnInt(0)]
				if f == nil {
					f = &fk{parent: st.ColumnText(1)}
					fks[st.ColumnInt(0)] = f
				}
				f.from = append(f.from, st.ColumnText(2))
				f.to = append(f.to, st.ColumnText(3))
				return nil
			})
		if err != nil {
			return err
		}
		for _, f := range fks {
			if f.to[0] == "" { // the parent's primary key
				f.to = f.to[:0]
				if err := sqlite.Query(c, `SELECT name FROM pragma_table_info(`+sqltext.QuoteString(f.parent)+`) WHERE pk > 0 ORDER BY pk`,
					func(st *sqlite3.Stmt) error { f.to = append(f.to, st.ColumnText(0)); return nil }); err != nil {
					return err
				}
			}
			var keys, match, notNull []string
			changes := false
			for i, to := range f.to {
				if i >= len(f.from) {
					break
				}
				_, coll, _, _, _, err := c.TableColumnMetadata("main", f.parent, to)
				if err != nil {
					return err
				}
				target, _ := m.Target(coll)
				changes = changes || sqltext.Fold(target) != sqltext.Fold(coll)
				k := fmt.Sprintf("k%d", i)
				keys = append(keys, sqltext.Quote(to)+" COLLATE "+target+" AS "+k)
				match = append(match, k+" COLLATE "+target+" = c."+sqltext.Quote(f.from[i]))
				notNull = append(notNull, "c."+sqltext.Quote(f.from[i])+" IS NOT NULL")
			}
			if !changes {
				continue
			}
			// The parent's keys under the new collation, gathered once, so the check takes one pass over each table.
			q := `WITH narcissus_parent_keys AS MATERIALIZED (SELECT DISTINCT ` + strings.Join(keys, ", ") + ` FROM ` + sqltext.Quote(f.parent) + ` NOT INDEXED)
				SELECT count(*) FROM ` + sqltext.Quote(o.Name) + ` AS c NOT INDEXED WHERE ` + strings.Join(notNull, " AND ") +
				` AND NOT EXISTS (SELECT 1 FROM narcissus_parent_keys WHERE ` + strings.Join(match, " AND ") + `)`
			orphans := 0
			if err := sqlite.Query(c, q, func(st *sqlite3.Stmt) error { orphans = st.ColumnInt(0); return nil }); err != nil {
				p.Problems = append(p.Problems, fmt.Sprintf("the foreign key from %s to %s couldn't be checked: %v", o.Name, f.parent, err))
				continue
			}
			if orphans > 0 {
				p.Problems = append(p.Problems, fmt.Sprintf("%d rows of %s wouldn't match a row of %s through their foreign key under the new collation (this counts any that already don't, which Narcissus can't tell apart without the app's collation)", orphans, o.Name, f.parent))
			}
		}
	}
	return nil
}

// checkSchema builds the converted schema in an empty database, so anything
// that wouldn't load shows up in the dry run, not half-way through a copy.
func checkSchema(c *sqlite3.Conn, p *Plan, _ Report, m Mapping, objs []object) error {
	shadows, err := shadowTables(c)
	if err != nil {
		return err
	}
	mem, err := sqlite.OpenMemory(context.Background())
	if err != nil {
		return err
	}
	defer mem.Close()
	if err := sqlite.AllowDoubleQuotedStrings(mem, true); err != nil {
		return err
	}
	err = createTables(mem, objs, shadows, m)
	for _, kind := range []string{"index", "view", "trigger"} {
		for _, o := range objs {
			if err == nil && o.Type == kind {
				err = exec(mem, o, m)
			}
		}
	}
	if err != nil {
		p.Problems = append(p.Problems, "the converted schema wouldn't load: "+err.Error())
	}
	return nil
}

func checkStats(c *sqlite3.Conn, p *Plan, _ Report, _ Mapping, _ []object) error {
	has := false
	err := sqlite.Query(c, `SELECT 1 FROM sqlite_schema WHERE name = 'sqlite_stat1'`, func(*sqlite3.Stmt) error { has = true; return nil })
	if has {
		p.Notes = append(p.Notes, "query planner statistics aren't copied; run ANALYZE on the new file if your app relies on them")
	}
	return err
}
