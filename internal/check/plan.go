package check

import (
	"slices"
	"strings"

	"github.com/ncruces/go-sqlite3"

	"github.com/kistdb/narcissus/internal/sqlite"
	"github.com/kistdb/narcissus/internal/sqltext"
)

// target is a query with what's needed to read its plans.
type target struct {
	Query
	tables  map[string]bool   // ordinary tables, case-folded
	aliases map[string]string // the query's aliases for tables, case-folded
}

// explain returns the steps of the query's plan.
func explain(c *sqlite3.Conn, sql string) ([]string, error) {
	var steps []string
	err := sqlite.Query(c, "EXPLAIN QUERY PLAN "+sql, func(st *sqlite3.Stmt) error {
		steps = append(steps, st.ColumnText(3))
		return nil
	})
	return steps, err
}

// analyze returns the steps that break a rule the query doesn't allow, and
// counts the steps that read all of a table's index.
func (t target) analyze(steps []string) (ps []Problem, indexScans int) {
	subqueries := make(map[string]bool)
	for _, s := range steps {
		for _, p := range []string{"CO-ROUTINE ", "MATERIALIZE "} {
			if name, ok := strings.CutPrefix(s, p); ok {
				subqueries[sqltext.Fold(name)] = true
			}
		}
	}
	for _, s := range steps {
		var r Rule
		name, viaIndex, scan := scanned(s)
		switch {
		case scan && !subqueries[name] && t.isTable(name):
			if viaIndex {
				indexScans++
				continue
			}
			r = FullScan
		case strings.HasPrefix(s, "USE TEMP B-TREE "):
			r = TempBTree
		case strings.Contains(s, " USING AUTOMATIC "):
			r = AutomaticIndex
		default:
			continue
		}
		if !t.allows(r) {
			ps = append(ps, Problem{Rule: r, Detail: s})
		}
	}
	return ps, indexScans
}

func (t target) allows(r Rule) bool { return slices.Contains(t.Allow, r) }

// isTable reports whether name, as a plan step gives it, is an ordinary table or an alias for one.
func (t target) isTable(name string) bool {
	if table, ok := t.aliases[name]; ok {
		name = table
	}
	return t.tables[name]
}

// scanned parses a "SCAN name [USING ...]" step, returning the case-folded name.
func scanned(step string) (name string, viaIndex, ok bool) {
	rest, ok := strings.CutPrefix(step, "SCAN ")
	if !ok {
		return "", false, false
	}
	name, using, viaIndex := strings.Cut(rest, " USING ")
	return sqltext.Fold(name), viaIndex && strings.Contains(using, "INDEX"), true
}

// usesIndex reports whether a plan step uses the named index.
func usesIndex(step, name string) bool {
	s, needle := sqltext.Fold(step), "index "+sqltext.Fold(name)
	for i := strings.Index(s, needle); i >= 0; {
		end := i + len(needle)
		if end == len(s) || s[end] == ' ' {
			return true
		}
		next := strings.Index(s[end:], needle)
		if next < 0 {
			break
		}
		i = end + next
	}
	return false
}

// tableNames returns the database's ordinary tables, case-folded.
func tableNames(c *sqlite3.Conn) (map[string]bool, error) {
	tables := make(map[string]bool)
	err := sqlite.Query(c, `SELECT name FROM pragma_table_list WHERE schema = 'main' AND type = 'table'`, func(st *sqlite3.Stmt) error {
		tables[sqltext.Fold(st.ColumnText(0))] = true
		return nil
	})
	return tables, err
}

// aliases maps the aliases a query gives tables, as in "orders o" or
// "orders AS o", to the tables. It's lexical, so it may hold extra names.
func aliases(sql string, tables map[string]bool) map[string]string {
	toks, err := sqltext.Lex(sql)
	if err != nil {
		return nil
	}
	isName := func(t sqltext.Token) bool {
		return t.Kind == sqltext.Quoted || t.Kind == sqltext.Word && !sqltext.IsKeyword(t.Text)
	}
	m := make(map[string]string)
	for i, tok := range toks {
		table := sqltext.Fold(tok.Ident())
		if !isName(tok) || !tables[table] {
			continue
		}
		j := i + 1
		if j < len(toks) && toks[j].Is("as") {
			j++
		}
		if j < len(toks) && isName(toks[j]) {
			m[sqltext.Fold(toks[j].Ident())] = table
		}
	}
	return m
}
