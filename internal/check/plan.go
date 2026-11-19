package check

import (
	"regexp"
	"slices"
	"strings"

	"github.com/ncruces/go-sqlite3"

	"github.com/kistdb/narcissus/internal/sqlite"
	"github.com/kistdb/narcissus/internal/sqltext"
)

// target is a query with what's needed to read its plans.
type target struct {
	Query
	aliases map[string]string // alias to name, from the query and the views it may use
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
		case scan && t.isTable(name, subqueries):
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

var valuesClause = regexp.MustCompile(`^\d+-row values clause$`)

// isTable reports whether a scanned name is a table: anything but a subquery,
// CTE or VALUES list, directly or through an alias.
func (t target) isTable(name string, subqueries map[string]bool) bool {
	if subqueries[name] || name == "constant row" || valuesClause.MatchString(name) {
		return false
	}
	if target, ok := t.aliases[name]; ok && subqueries[target] {
		return false
	}
	return true
}

// scanned parses a "SCAN name [USING ...]" step, returning the case-folded
// name. Virtual tables and table-valued functions aren't scans of a table.
func scanned(step string) (name string, viaIndex, ok bool) {
	rest, ok := strings.CutPrefix(step, "SCAN ")
	if !ok || strings.Contains(rest, " VIRTUAL TABLE") {
		return "", false, false
	}
	rest, using, viaIndex := strings.Cut(rest, " USING ")
	for _, suffix := range []string{" LEFT-JOIN", " RIGHT-JOIN", " EXISTS"} {
		rest = strings.TrimSuffix(rest, suffix)
	}
	name = sqltext.Fold(rest)
	for _, schema := range []string{"main.", "temp."} {
		name = strings.TrimPrefix(name, schema)
	}
	return name, viaIndex && strings.Contains(using, "INDEX"), true
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

// aliases maps the aliases in sql, as in "orders o" or "orders AS o", to
// what they name. It's lexical, so it may hold harmless extra names.
func aliases(sql string) map[string]string {
	toks, err := sqltext.Lex(sql)
	if err != nil {
		return nil
	}
	isName := func(t sqltext.Token) bool {
		return t.Kind == sqltext.Quoted || t.Kind == sqltext.Word && !sqltext.IsKeyword(t.Text)
	}
	m := make(map[string]string)
	for i, tok := range toks {
		if !isName(tok) {
			continue
		}
		j := i + 1
		if j < len(toks) && toks[j].Is("as") {
			j++
		}
		if j < len(toks) && isName(toks[j]) {
			m[sqltext.Fold(toks[j].Ident())] = sqltext.Fold(tok.Ident())
		}
	}
	return m
}

// viewSQL returns the definitions of the database's views, joined.
func viewSQL(c *sqlite3.Conn) (string, error) {
	var b strings.Builder
	err := sqlite.Query(c, `SELECT sql FROM sqlite_schema WHERE type = 'view'`, func(st *sqlite3.Stmt) error {
		b.WriteString(st.ColumnText(0) + ";\n")
		return nil
	})
	return b.String(), err
}
