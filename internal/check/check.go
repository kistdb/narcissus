// Package check finds queries whose plans read whole tables, sort in
// temporary b-trees or build automatic indexes, and suggests indexes to fix them.
package check

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ncruces/go-sqlite3"

	"github.com/kistdb/narcissus/internal/index"
	"github.com/kistdb/narcissus/internal/scratch"
	"github.com/kistdb/narcissus/internal/sqlite"
	"github.com/kistdb/narcissus/internal/sqltext"
)

// Rule is a kind of plan step that check reports.
type Rule string

// The rules.
const (
	FullScan       Rule = "full-scan"       // reads every row of a table
	TempBTree      Rule = "temp-btree"      // sorts or groups in a temporary b-tree
	AutomaticIndex Rule = "automatic-index" // builds a throwaway index on every run
)

var rules = []Rule{FullScan, TempBTree, AutomaticIndex}

// Query is a query from the queries file.
type Query struct {
	Name  string // from a "-- name:" comment; may be empty
	File  string
	Line  int
	SQL   string
	Allow []Rule // from a "-- narcissus: allow" comment
}

// Problem is a plan step that breaks a rule.
type Problem struct {
	Rule   Rule
	Detail string // the step, as EXPLAIN QUERY PLAN words it
}

// Result is the outcome for one query.
type Result struct {
	Query    Query
	Err      error // the query didn't prepare
	Problems []Problem
	Suggest  []string // CREATE INDEX statements that fix the problems
}

// Failed reports whether the query failed the check.
func (r Result) Failed() bool { return r.Err != nil || len(r.Problems) > 0 }

// Report is the outcome of a check.
type Report struct {
	Results []Result
	Unused  []index.Index // desired, non-unique indexes no query uses
}

// Failed reports whether any query failed.
func (r Report) Failed() bool {
	return slices.ContainsFunc(r.Results, Result.Failed)
}

// ParseQueries parses a queries file: statements, each optionally preceded by
// "-- name: x" and "-- narcissus: allow rule, ..." comment lines.
func ParseQueries(file, src string) ([]Query, error) {
	stmts, err := sqltext.Split(file, src)
	if err != nil {
		return nil, err
	}
	names := make(map[string]int)
	qs := make([]Query, 0, len(stmts))
	for _, st := range stmts {
		q := Query{File: file, Line: st.Line, SQL: st.SQL()}
		if err := q.directives(st.Comments); err != nil {
			return nil, &sqltext.Error{File: file, Line: st.Line, Err: err}
		}
		if line, ok := names[q.Name]; ok && q.Name != "" {
			return nil, &sqltext.Error{File: file, Line: st.Line, Err: fmt.Errorf("query %s already named on line %d", q.Name, line)}
		}
		names[q.Name] = st.Line
		qs = append(qs, q)
	}
	return qs, nil
}

func (q *Query) directives(comments []string) error {
	for _, c := range comments {
		key, value, ok := strings.Cut(c, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "name":
			q.Name = value
		case "narcissus":
			allow, ok := strings.CutPrefix(strings.ToLower(value), "allow ")
			if !ok {
				return fmt.Errorf("unknown directive %q (want allow)", value)
			}
			for r := range strings.SplitSeq(allow, ",") {
				r := Rule(strings.TrimSpace(r))
				if !slices.Contains(rules, r) {
					return fmt.Errorf("unknown rule %q (want %s, %s or %s)", r, FullScan, TempBTree, AutomaticIndex)
				}
				q.Allow = append(q.Allow, r)
			}
		}
	}
	return nil
}

// Run checks each query's plan against the scratch database.
func Run(db *scratch.DB, queries []Query) (*Report, error) {
	views, err := viewSQL(db.Conn)
	if err != nil {
		return nil, err
	}
	rep := &Report{}
	var allSteps []string
	for _, q := range queries {
		res := Result{Query: q}
		t := target{Query: q, aliases: aliases(q.SQL + ";\n" + views)}
		steps, err := explain(db.Conn, q.SQL)
		if err != nil {
			res.Err = err
			rep.Results = append(rep.Results, res)
			continue
		}
		allSteps = append(allSteps, steps...)
		res.Problems, _ = t.analyze(steps)
		if len(res.Problems) > 0 {
			if res.Suggest, err = suggest(db.Conn, t); err != nil {
				return nil, fmt.Errorf("%s:%d: suggest indexes: %w", q.File, q.Line, err)
			}
		}
		rep.Results = append(rep.Results, res)
	}
	if len(queries) > 0 {
		for _, ix := range db.Indexes {
			if ix.Unique || ix.AllowUnused || slices.ContainsFunc(allSteps, func(s string) bool { return usesIndex(s, ix.Name) }) {
				continue
			}
			fk, err := backsForeignKey(db.Conn, ix)
			if err != nil {
				return nil, err
			}
			if !fk {
				rep.Unused = append(rep.Unused, ix)
			}
		}
	}
	return rep, nil
}

// backsForeignKey reports whether the index's leading columns are a foreign
// key's, so it serves lookups that query plans don't show.
func backsForeignKey(c *sqlite3.Conn, ix index.Index) (bool, error) {
	var cols []string
	err := sqlite.Query(c, `SELECT name FROM pragma_index_info(`+sqltext.QuoteString(ix.Name)+`) ORDER BY seqno`,
		func(st *sqlite3.Stmt) error { cols = append(cols, sqltext.Fold(st.ColumnText(0))); return nil })
	if err != nil {
		return false, err
	}
	fks := make(map[int][]string)
	err = sqlite.Query(c, `SELECT id, "from" FROM pragma_foreign_key_list(`+sqltext.QuoteString(ix.Table)+`) ORDER BY id, seq`,
		func(st *sqlite3.Stmt) error {
			fks[st.ColumnInt(0)] = append(fks[st.ColumnInt(0)], sqltext.Fold(st.ColumnText(1)))
			return nil
		})
	for _, from := range fks {
		if len(from) <= len(cols) && slices.Equal(cols[:len(from)], from) {
			return true, err
		}
	}
	return false, err
}
