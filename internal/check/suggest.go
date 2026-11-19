package check

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/ncruces/go-sqlite3"

	"github.com/kistdb/narcissus/internal/sqlite"
	"github.com/kistdb/narcissus/internal/sqltext"
)

const (
	maxColumns     = 3 // per suggested index
	maxSuggestions = 3 // per query
	beamWidth      = 3 // candidates kept at each column count
	maxPairColumns = 8 // columns per table tried in pairs
)

// candidate is an index to try: a table and its key columns.
type candidate struct {
	table   string
	columns []string
}

// tried is a candidate with the cost of the plan when it exists.
type tried struct {
	candidate
	cost int
}

// suggest finds up to maxSuggestions indexes that, added in turn, each cut
// the query's cost. It leaves the database unchanged.
func suggest(c *sqlite3.Conn, t target) (_ []string, err error) {
	read, err := columnsRead(c, t.SQL)
	if err != nil {
		return nil, err
	}
	if err := c.Exec(`SAVEPOINT narcissus_suggest`); err != nil {
		return nil, err
	}
	defer func() {
		if rerr := c.Exec(`ROLLBACK TO narcissus_suggest; RELEASE narcissus_suggest`); err == nil {
			err = rerr
		}
	}()

	cur, err := cost(c, t)
	if err != nil {
		return nil, err
	}
	trial, err := sqlite.FreeName(c, "narcissus_candidate")
	if err != nil {
		return nil, err
	}
	var out []string
	for len(out) < maxSuggestions && cur > 0 {
		best, err := search(c, t, read, trial)
		if err != nil {
			return nil, err
		}
		picks := []candidate{}
		switch {
		case best != nil && best.cost < cur:
			picks, cur = append(picks, best.candidate), best.cost
		case len(out)+2 <= maxSuggestions:
			// No one index helps, as in a join that needs one on each side.
			pair, n, err := searchPairs(c, t, read, trial)
			if err != nil {
				return nil, err
			}
			if pair == nil || n >= cur {
				return out, nil
			}
			picks, cur = pair, n
		default:
			return out, nil
		}
		for _, p := range picks {
			name, err := sqlite.FreeName(c, p.name())
			if err != nil {
				return nil, err
			}
			sql := p.sql(name)
			if err := c.Exec(sql); err != nil {
				return nil, err
			}
			out = append(out, sql+";")
		}
	}
	return out, nil
}

// searchPairs tries a one-column index on each of two tables together, and
// returns the cheapest pair, or nil if none could be built.
func searchPairs(c *sqlite3.Conn, t target, read map[string][]string, trial string) ([]candidate, int, error) {
	tables := slices.Sorted(maps.Keys(read))
	second, err := sqlite.FreeName(c, trial+"_2")
	if err != nil {
		return nil, 0, err
	}
	var best []candidate
	bestCost := 0
	for i, a := range tables {
		for _, b := range tables[i+1:] {
			for _, ca := range read[a][:min(len(read[a]), maxPairColumns)] {
				for _, cb := range read[b][:min(len(read[b]), maxPairColumns)] {
					pair := []candidate{{a, []string{ca}}, {b, []string{cb}}}
					n, ok, err := tryAll(c, t, pair, []string{trial, second})
					if err != nil {
						return nil, 0, err
					}
					if ok && (best == nil || n < bestCost) {
						best, bestCost = pair, n
					}
				}
			}
		}
	}
	return best, bestCost, nil
}

// cost is the number of problems in the query's plan, where reading all of
// an index counts as a full scan unless the query allows those.
func cost(c *sqlite3.Conn, t target) (int, error) {
	steps, err := explain(c, t.SQL)
	if err != nil {
		return 0, err
	}
	ps, indexScans := t.analyze(steps)
	if t.allows(FullScan) {
		indexScans = 0
	}
	return len(ps) + indexScans, nil
}

// search returns the cheapest candidate, growing the beamWidth cheapest of
// each column count by one column, or nil if no candidate could be built.
func search(c *sqlite3.Conn, t target, read map[string][]string, trial string) (*tried, error) {
	var level []tried
	for _, table := range slices.Sorted(maps.Keys(read)) {
		for _, col := range read[table] {
			level = append(level, tried{candidate: candidate{table, []string{col}}})
		}
	}
	var best *tried
	for n := 1; n <= maxColumns && len(level) > 0; n++ {
		kept := level[:0]
		for _, cand := range level {
			cost, ok, err := try(c, t, cand.candidate, trial)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			cand.cost = cost
			kept = append(kept, cand)
			if best == nil || cost < best.cost {
				best = &cand
			}
		}
		slices.SortStableFunc(kept, func(a, b tried) int { return cmp.Compare(a.cost, b.cost) })
		level = grow(kept[:min(beamWidth, len(kept))], read)
	}
	return best, nil
}

// grow extends each candidate by each column of its table it lacks.
func grow(level []tried, read map[string][]string) []tried {
	var next []tried
	for _, t := range level {
		for _, col := range read[t.table] {
			if !slices.Contains(t.columns, col) {
				next = append(next, tried{candidate: candidate{t.table, append(slices.Clip(t.columns), col)}})
			}
		}
	}
	return next
}

// try costs the plan with the candidate index added, and reports false if
// the index can't be built, as on a virtual table.
func try(c *sqlite3.Conn, t target, cand candidate, name string) (int, bool, error) {
	return tryAll(c, t, []candidate{cand}, []string{name})
}

// tryAll costs the plan with all the candidates added, under the given names.
func tryAll(c *sqlite3.Conn, t target, cands []candidate, names []string) (_ int, ok bool, err error) {
	if err := c.Exec(`SAVEPOINT narcissus_try`); err != nil {
		return 0, false, err
	}
	defer func() {
		if rerr := c.Exec(`ROLLBACK TO narcissus_try; RELEASE narcissus_try`); err == nil {
			err = rerr
		}
	}()
	for i, cand := range cands {
		if c.Exec(cand.sql(names[i])) != nil {
			return 0, false, nil
		}
	}
	n, err := cost(c, t)
	return n, err == nil, err
}

// columnsRead returns the columns the query reads, by table, in the order SQLite resolves them.
func columnsRead(c *sqlite3.Conn, sql string) (_ map[string][]string, err error) {
	read := make(map[string][]string)
	err = c.SetAuthorizer(func(action sqlite3.AuthorizerActionCode, table, column, schema, _ string) sqlite3.AuthorizerReturnCode {
		if action == sqlite3.AUTH_READ && schema == "main" && column != "" && !strings.HasPrefix(table, "sqlite_") &&
			!slices.Contains(read[table], column) {
			read[table] = append(read[table], column)
		}
		return sqlite3.AUTH_OK
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		if rerr := c.SetAuthorizer(nil); err == nil {
			err = rerr
		}
	}()
	st, _, err := c.Prepare(sql)
	if err != nil {
		return nil, err
	}
	return read, st.Close()
}

func (cand candidate) sql(name string) string {
	cols := make([]string, len(cand.columns))
	for i, col := range cand.columns {
		cols[i] = sqltext.Quote(col)
	}
	return "CREATE INDEX " + sqltext.Quote(name) + " ON " + sqltext.Quote(cand.table) + " (" + strings.Join(cols, ", ") + ")"
}

var nonWord = regexp.MustCompile(`[^A-Za-z0-9_]+`)

func (cand candidate) name() string {
	return nonWord.ReplaceAllString(cand.table+"_"+strings.Join(cand.columns, "_"), "_")
}
