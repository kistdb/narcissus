package fleet

import (
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/ncruces/go-sqlite3"

	"github.com/kistdb/narcissus/internal/index"
	"github.com/kistdb/narcissus/internal/sqlite"
	"github.com/kistdb/narcissus/internal/sqltext"
)

// vetter decides which indexes Narcissus builds: only those whose keys every
// SQLite computes the same way, since the app's SQLite may differ from ours.
type vetter struct {
	c     *sqlite3.Conn
	allow bool // NOCASE, and lower(), upper() and trim() of text columns
	utf8  bool // text conversions differ between versions in UTF-16 databases
}

func newVetter(c *sqlite3.Conn, allow bool) (*vetter, error) {
	v := &vetter{c: c, allow: allow}
	err := sqlite.Query(c, `PRAGMA encoding`, func(st *sqlite3.Stmt) error {
		v.utf8 = st.ColumnText(0) == "UTF-8"
		return nil
	})
	return v, err
}

// findings collects what an index depends on: what the flag would allow,
// and what no flag can make safe.
type findings struct {
	shape           error // the index isn't a kind Narcissus builds
	needFlag, never []string
}

func (f *findings) collation(v *vetter, name, where string) {
	switch n := sqltext.Fold(name); {
	case n == "binary":
	case n != "nocase": // RTRIM sorts differently before 3.29 (RHEL 8 ships 3.26)
		f.never = append(f.never, "COLLATE "+name+where)
	case !v.allow:
		f.needFlag = append(f.needFlag, "COLLATE "+name+where)
	}
}

// vet checks an index's shape, and what SQLite reports it reads, before it's built.
func (v *vetter) vet(ix index.Index, r sqlite.Reads) error {
	var f findings
	keys, conds, err := ix.Shape()
	if err != nil {
		f.shape = err
		return f.err()
	}
	for _, k := range keys {
		if k.Collate != "" {
			f.collation(v, k.Collate, "")
		}
		if len(k.Funcs) == 0 {
			continue
		}
		if !v.utf8 {
			f.never = append(f.never, k.Describe()+" in a UTF-16 database")
		}
		if !v.allow {
			f.needFlag = append(f.needFlag, k.Describe())
		}
		text, err := v.textColumn(ix.Table, k.Column)
		if err != nil {
			return err
		}
		if !text {
			f.never = append(f.never, k.Describe()+" on a column that isn't text")
		} else if err := v.realInText(&f, ix.Table, k.Column); err != nil {
			return err
		}
	}
	for _, c := range conds {
		switch c.Literal {
		case index.Text:
			text, err := v.textColumn(ix.Table, c.Column)
			if err != nil {
				return err
			}
			if !text {
				f.never = append(f.never, "text compared with "+c.Column+", which isn't a text column")
			} else if err := v.realInText(&f, ix.Table, c.Column); err != nil {
				return err
			}
			if !v.utf8 {
				f.never = append(f.never, "text compared with "+c.Column+" in a UTF-16 database")
			}
		case index.Integer:
			if err := v.integerCompare(&f, ix.Table, c.Column); err != nil {
				return err
			}
		}
	}
	for _, fn := range r.Functions { // a second net under Shape
		if !slices.Contains([]string{"lower", "upper", "trim", "ltrim", "rtrim"}, sqltext.Fold(fn)) {
			f.never = append(f.never, fn+"()")
		}
	}
	tables := map[string]bool{ix.Table: true}
	for _, tc := range r.Columns {
		tables[tc[0]] = true
		if err := v.column(&f, tc[0], tc[1]); err != nil {
			return err
		}
	}
	for t := range tables {
		if err := v.rowidKey(&f, t); err != nil {
			return err
		}
	}
	return f.err()
}

// column checks a column's own collation, and that it's stored, not computed.
func (v *vetter) column(f *findings, table, col string) error {
	_, coll, _, _, _, err := v.c.TableColumnMetadata("main", table, col)
	if err != nil {
		return fmt.Errorf("column %s.%s: %w", table, col, err)
	}
	f.collation(v, coll, " (column "+col+")")
	virtual := false
	err = sqlite.Query(v.c, `SELECT 1 FROM pragma_table_xinfo(`+sqltext.QuoteString(table)+`) WHERE name = `+sqltext.QuoteString(col)+` AND hidden = 2`,
		func(*sqlite3.Stmt) error { virtual = true; return nil })
	if virtual {
		f.never = append(f.never, "virtual generated column "+col)
	}
	if err != nil {
		return err
	}
	return v.columnDefault(f, table, col)
}

// plainDefault matches defaults every SQLite reads the same way: NULL, text,
// and numbers of at most 15 digits in all, without an exponent.
var plainDefault = regexp.MustCompile(`^(NULL|'[^']*'|-?[0-9]{1,15}|-?[0-9]+\.[0-9]+)$`)

// columnDefault declines a column whose default isn't a plain value: rows from
// before an ADD COLUMN don't store it, so each SQLite parses it again.
func (v *vetter) columnDefault(f *findings, table, col string) error {
	var dflt sql.NullString
	err := sqlite.Query(v.c, `SELECT dflt_value FROM pragma_table_xinfo(`+sqltext.QuoteString(table)+`) WHERE name = `+sqltext.QuoteString(col),
		func(st *sqlite3.Stmt) error {
			if st.ColumnType(0) != sqlite3.NULL {
				dflt = sql.NullString{String: st.ColumnText(0), Valid: true}
			}
			return nil
		})
	if err != nil || !dflt.Valid {
		return err
	}
	d := strings.TrimSpace(dflt.String)
	text, err := v.textColumn(table, col)
	if err != nil {
		return err
	}
	digits := len(strings.NewReplacer("-", "", ".", "").Replace(d))
	if !plainDefault.MatchString(strings.ToUpper(d)) || !strings.HasPrefix(d, "'") && digits > 15 ||
		strings.HasPrefix(d, "'") && !text && !v.untyped(table, col) {
		f.never = append(f.never, "column "+col+" with the default "+d)
	}
	return nil
}

func (v *vetter) untyped(table, col string) bool {
	declType, _, _, _, _, err := v.c.TableColumnMetadata("main", table, col)
	return err == nil && strings.TrimSpace(declType) == ""
}

// integerCompare declines a whole-number comparison with a numeric column that
// holds text, which SQLite versions compare differently (as with "5\x00").
func (v *vetter) integerCompare(f *findings, table, col string) error {
	text, err := v.textColumn(table, col)
	if err != nil || text {
		return err
	}
	declType, _, _, _, _, err := v.c.TableColumnMetadata("main", table, col)
	if err != nil || declType == "" { // untyped columns compare text as text
		return err
	}
	found := false
	err = sqlite.Query(v.c, `SELECT 1 FROM `+sqltext.Quote(table)+` WHERE typeof(`+sqltext.Quote(col)+`) = 'text' LIMIT 1`,
		func(*sqlite3.Stmt) error { found = true; return nil })
	if found {
		f.never = append(f.never, "a number compared with "+col+", which holds text in some rows")
	}
	return err
}

// realInText declines a text column that holds decimal numbers in some rows:
// turned into text, they get different digits in different SQLite versions.
func (v *vetter) realInText(f *findings, table, col string) error {
	found := false
	err := sqlite.Query(v.c, `SELECT 1 FROM `+sqltext.Quote(table)+` WHERE typeof(`+sqltext.Quote(col)+`) = 'real' LIMIT 1`,
		func(*sqlite3.Stmt) error { found = true; return nil })
	if found {
		f.never = append(f.never, "text column "+col+", which holds decimal numbers in some rows")
	}
	return err
}

// textColumn reports whether a column has TEXT affinity, so it holds text, not numbers.
func (v *vetter) textColumn(table, col string) (bool, error) {
	declType, _, _, _, _, err := v.c.TableColumnMetadata("main", table, col)
	if err != nil {
		return false, fmt.Errorf("column %s.%s: %w", table, col, err)
	}
	t := strings.ToUpper(declType)
	return !strings.Contains(t, "INT") && (strings.Contains(t, "CHAR") || strings.Contains(t, "CLOB") || strings.Contains(t, "TEXT")), nil
}

// rowidKey checks the primary key a WITHOUT ROWID table adds to its indexes.
func (v *vetter) rowidKey(f *findings, table string) error {
	var cols []string
	err := sqlite.Query(v.c, `SELECT i.name FROM pragma_table_list AS l, pragma_table_info(l.name) AS i
		WHERE l.schema = 'main' AND l.name = `+sqltext.QuoteString(table)+` AND l.wr AND i.pk > 0`,
		func(st *sqlite3.Stmt) error { cols = append(cols, st.ColumnText(0)); return nil })
	if err != nil {
		return err
	}
	for _, col := range cols {
		_, coll, _, _, _, err := v.c.TableColumnMetadata("main", table, col)
		if err != nil {
			return err
		}
		f.collation(v, coll, " (primary key "+col+")")
	}
	return nil
}

// built checks the collations SQLite actually used in the index, once built.
func (v *vetter) built(ix index.Index) error {
	var f findings
	err := sqlite.Query(v.c, `SELECT DISTINCT coll FROM pragma_index_xinfo(`+sqltext.QuoteString(ix.Name)+`) WHERE coll IS NOT NULL`,
		func(st *sqlite3.Stmt) error { f.collation(v, st.ColumnText(0), ""); return nil })
	if err != nil {
		return err
	}
	return f.err()
}

func (f findings) err() error {
	switch {
	case f.shape != nil:
		return fmt.Errorf("has %s, and Narcissus only builds indexes on columns (with lower(), upper() or trim() "+
			"if allowed) whose WHERE compares columns with whole numbers, text or NULL. Please create this one from your app", f.shape)
	case len(f.never) > 0:
		return fmt.Errorf("uses %s. Narcissus only builds indexes every SQLite computes the same way; please create this one from your app",
			strings.Join(dedupe(f.never), ", "))
	case len(f.needFlag) > 0:
		return fmt.Errorf("depends on %s, which an app can redefine. "+
			"If your app uses SQLite's standard versions, run again with -allow-collations-and-functions",
			strings.Join(dedupe(f.needFlag), ", "))
	}
	return nil
}

func dedupe(s []string) []string {
	var out []string
	for _, x := range s {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// Refusals returns why apply would decline to build each index on c, as it stands.
func Refusals(c *sqlite3.Conn, ixs []index.Index) ([]error, error) {
	v, err := newVetter(c, false)
	if err != nil {
		return nil, err
	}
	var refused []error
	for _, ix := range ixs {
		name, err := sqlite.FreeName(c, "narcissus_check_"+ix.Name)
		if err != nil {
			return nil, err
		}
		sql, err := ix.Renamed(name) // the index itself still holds its name
		if err == nil {
			err = sqlite.PrepareIndexChange(c, sql, func(r sqlite.Reads) error { return v.vet(ix, r) })
		}
		if err != nil {
			refused = append(refused, fmt.Errorf("%s: %w", ix.Name, err))
		}
	}
	return refused, nil
}
