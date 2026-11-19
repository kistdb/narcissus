package collate

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ncruces/go-sqlite3"

	"github.com/kistdb/narcissus/internal/sqlite"
	"github.com/kistdb/narcissus/internal/sqltext"
)

// Write converts the database on src, which holds the plan's read transaction,
// into a new file at out. The file only appears at out once it has passed every check.
func Write(ctx context.Context, src *sqlite3.Conn, srcPath, out string, p Plan, m Mapping) (err error) {
	if p.Blocked() {
		return errors.New("the plan has problems to resolve first")
	}
	if _, err := os.Lstat(out); err == nil {
		return fmt.Errorf("%s already exists; please choose a new file", out)
	}
	info, err := os.Stat(srcPath)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(out), "."+filepath.Base(out)+".narcissus-tmp")
	dst, err := sqlite.Create(ctx, tmp, info.Mode().Perm())
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s is left from an interrupted run; please delete it and try again", tmp)
	}
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, dst.Close())
		if err == nil {
			err = place(tmp, out)
		}
		err = errors.Join(err, removeIfThere(tmp), removeSide(tmp))
	}()
	if err := copyHeader(src, dst); err != nil {
		return err
	}
	tables, err := copyContent(src, dst, m)
	if err != nil {
		return err
	}
	if err := verify(src, dst, p, tables); err != nil {
		return err
	}
	if wal, err := walMode(srcPath); err != nil || !wal {
		return err
	}
	return dst.Exec(`PRAGMA journal_mode = WAL`)
}

// place puts the finished file at out without replacing anything there.
func place(tmp, out string) error {
	err := os.Link(tmp, out)
	if err == nil || errors.Is(err, fs.ErrExist) {
		return err
	}
	// A file system without hard links, such as exFAT.
	if _, err := os.Lstat(out); err == nil {
		return fmt.Errorf("%s appeared while converting; the new file wasn't kept", out)
	}
	if err := os.Rename(tmp, out); err != nil {
		return err
	}
	return nil
}

func removeIfThere(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// walMode reads WAL mode from the file's header, which an immutable open hides.
func walMode(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	header := make([]byte, 20)
	if _, err := f.ReadAt(header, 0); err != nil {
		return false, err
	}
	return header[18] == 2, nil
}

// copyHeader carries over the settings a database must have before it holds tables.
func copyHeader(src, dst *sqlite3.Conn) error {
	for _, pragma := range []string{"page_size", "encoding", "auto_vacuum"} {
		var v string
		if err := sqlite.Query(src, "PRAGMA "+pragma, func(st *sqlite3.Stmt) error { v = st.ColumnText(0); return nil }); err != nil {
			return err
		}
		if err := dst.Exec("PRAGMA " + pragma + " = " + sqltext.QuoteString(v)); err != nil {
			return fmt.Errorf("PRAGMA %s: %w", pragma, err)
		}
	}
	return nil
}

// copyContent recreates the schema with new collations, and copies every row.
// It returns the tables whose rows it copied.
func copyContent(src, dst *sqlite3.Conn, m Mapping) (copied []string, err error) {
	objs, err := objects(src)
	if err != nil {
		return nil, err
	}
	shadows, err := shadowTables(src)
	if err != nil {
		return nil, err
	}
	if err := dst.Exec(`BEGIN`); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = dst.Exec(`ROLLBACK`)
		}
	}()
	// Tables first and rows next; indexes and triggers come after, so no trigger fires.
	if err := createTables(dst, objs, shadows, m); err != nil {
		return nil, err
	}
	for _, o := range objs {
		if o.Type != "table" || module(o) != "" {
			continue
		}
		if shadows[o.Name] {
			if err := dst.Exec(`DELETE FROM ` + sqltext.Quote(o.Name)); err != nil { // replace what the module wrote
				return nil, err
			}
		}
		if err := copyRows(src, dst, o.Name); err != nil {
			return nil, fmt.Errorf("table %s: %w", o.Name, err)
		}
		copied = append(copied, o.Name)
	}
	if err := copySequence(src, dst); err != nil {
		return nil, err
	}
	for _, kind := range []string{"index", "view", "trigger"} {
		for _, o := range objs {
			if o.Type == kind {
				if err := exec(dst, o, m); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, pragma := range []string{"user_version", "application_id"} {
		var v int64
		if err := sqlite.Query(src, "PRAGMA "+pragma, func(st *sqlite3.Stmt) error { v = st.ColumnInt64(0); return nil }); err != nil {
			return nil, err
		}
		if err := dst.Exec(fmt.Sprintf("PRAGMA %s = %d", pragma, v)); err != nil {
			return nil, err
		}
	}
	return copied, dst.Exec(`COMMIT`)
}

// createTables creates the tables: virtual tables first, which make their own
// shadow tables, then any shadow-named table no module made, such as external content.
func createTables(dst *sqlite3.Conn, objs []object, shadows map[string]bool, m Mapping) error {
	for _, o := range objs {
		if o.Type == "table" && !shadows[o.Name] {
			if err := exec(dst, o, m); err != nil {
				return err
			}
		}
	}
	for _, o := range objs {
		if o.Type != "table" || !shadows[o.Name] {
			continue
		}
		made := false
		err := sqlite.Query(dst, `SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = `+sqltext.QuoteString(o.Name),
			func(*sqlite3.Stmt) error { made = true; return nil })
		if err == nil && !made {
			err = exec(dst, o, m)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// exec creates an object from its statement, with its collations replaced.
func exec(dst *sqlite3.Conn, o object, m Mapping) error {
	sql, err := rewrite(o.SQL, m)
	if err == nil {
		err = dst.Exec(sql)
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", o.Type, o.Name, err)
	}
	return nil
}

// rewrite replaces each COLLATE name in sql by its mapped built-in collation,
// spaced so it can't run into the tokens around it.
func rewrite(sql string, m Mapping) (string, error) {
	toks, err := sqltext.Lex(sql)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	last := 0
	for i, t := range toks {
		if !t.Is("collate") || i+1 >= len(toks) || IsBuiltin(toks[i+1].Ident()) {
			continue
		}
		name := toks[i+1]
		to, ok := m.Target(name.Ident())
		if !ok {
			return "", fmt.Errorf("no replacement for collation %s", name.Ident())
		}
		b.WriteString(sql[last:name.Start])
		if name.Start == toks[i].End {
			b.WriteByte(' ')
		}
		b.WriteString(strings.ToUpper(to))
		if name.End < len(sql) && !strings.ContainsRune(" \t\r\n,;()", rune(sql[name.End])) {
			b.WriteByte(' ')
		}
		last = name.End
	}
	b.WriteString(sql[last:])
	return b.String(), nil
}

// shadowTables returns the tables virtual tables keep their data in.
func shadowTables(c *sqlite3.Conn) (map[string]bool, error) {
	shadows := make(map[string]bool)
	err := sqlite.Query(c, `SELECT name FROM pragma_table_list WHERE schema = 'main' AND type = 'shadow'`,
		func(st *sqlite3.Stmt) error { shadows[st.ColumnText(0)] = true; return nil })
	return shadows, err
}

// columns returns a table's columns to copy and compare (stored ones, and the
// rowid first unless it's WITHOUT ROWID), and its stored generated columns.
func columns(c *sqlite3.Conn, table string) (cols, generated []string, err error) {
	withoutRowid := false
	err = sqlite.Query(c, `SELECT name, hidden FROM pragma_table_xinfo(`+sqltext.QuoteString(table)+`) WHERE hidden IN (0, 3) ORDER BY cid`,
		func(st *sqlite3.Stmt) error {
			if st.ColumnInt(1) == 3 {
				generated = append(generated, sqltext.Quote(st.ColumnText(0)))
			} else {
				cols = append(cols, sqltext.Quote(st.ColumnText(0)))
			}
			return nil
		})
	if err == nil {
		err = sqlite.Query(c, `SELECT wr FROM pragma_table_list WHERE schema = 'main' AND name = `+sqltext.QuoteString(table),
			func(st *sqlite3.Stmt) error { withoutRowid = st.ColumnBool(0); return nil })
	}
	if err != nil || withoutRowid {
		return cols, generated, err
	}
	for _, alias := range []string{"rowid", "_rowid_", "oid"} {
		if !slices.ContainsFunc(append(cols, generated...), func(c string) bool { return sqltext.Fold(strings.Trim(c, `"`)) == alias }) {
			return append([]string{alias}, cols...), generated, nil
		}
	}
	return nil, nil, errors.New("its columns hide the rowid")
}

// exact selects each column as its type and value, with text as its stored bytes.
func exact(cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = "typeof(" + c + "), CASE typeof(" + c + ") WHEN 'text' THEN CAST(" + c + " AS BLOB) ELSE " + c + " END"
	}
	return strings.Join(parts, ", ")
}

// copyRows copies a table's stored columns and rowid exactly, without comparing
// anything: the old collation may not exist here.
func copyRows(src, dst *sqlite3.Conn, table string) error {
	cols, _, err := columns(src, table)
	if err != nil {
		return err
	}
	values := make([]string, len(cols))
	for i := range cols {
		values[i] = fmt.Sprintf("CASE ?%d WHEN 'text' THEN CAST(?%d AS TEXT) ELSE ?%d END", 2*i+1, 2*i+2, 2*i+2)
	}
	// OR ABORT: a table's own ON CONFLICT REPLACE or IGNORE must never drop a row.
	ins, _, err := dst.Prepare(`INSERT OR ABORT INTO ` + sqltext.Quote(table) + ` (` + strings.Join(cols, ", ") + `) VALUES (` + strings.Join(values, ", ") + `)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	return sqlite.Query(src, `SELECT `+exact(cols)+` FROM `+sqltext.Quote(table)+` NOT INDEXED`, func(row *sqlite3.Stmt) error {
		for i := range 2 * len(cols) {
			if err := sqlite.BindColumn(ins, i+1, row, i); err != nil {
				return err
			}
		}
		return ins.Exec()
	})
}

// copySequence keeps AUTOINCREMENT counters.
func copySequence(src, dst *sqlite3.Conn) error {
	has := func(c *sqlite3.Conn) (bool, error) {
		found := false
		err := sqlite.Query(c, `SELECT 1 FROM sqlite_schema WHERE name = 'sqlite_sequence'`, func(*sqlite3.Stmt) error { found = true; return nil })
		return found, err
	}
	inSrc, err := has(src)
	if err != nil || !inSrc {
		return err
	}
	inDst, err := has(dst)
	if err != nil || !inDst { // no AUTOINCREMENT tables left to count for
		return err
	}
	if err := dst.Exec(`DELETE FROM sqlite_sequence`); err != nil {
		return err
	}
	ins, _, err := dst.Prepare(`INSERT INTO sqlite_sequence (name, seq) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	return sqlite.Query(src, `SELECT name, seq FROM sqlite_sequence`, func(row *sqlite3.Stmt) error {
		if err := errors.Join(sqlite.BindColumn(ins, 1, row, 0), sqlite.BindColumn(ins, 2, row, 1)); err != nil {
			return err
		}
		return ins.Exec()
	})
}

// fingerprint summarises a table's rows, in any order: their count, and the sum
// and XOR of a hash of each row's exact values.
type fingerprint struct{ rows, sum, xor uint64 }

func tableFingerprint(c *sqlite3.Conn, table string) (fingerprint, error) {
	var fp fingerprint
	cols, generated, err := columns(c, table)
	if err != nil {
		return fp, err
	}
	all := append(cols, generated...)
	err = sqlite.Query(c, `SELECT `+exact(all)+` FROM `+sqltext.Quote(table)+` NOT INDEXED`, func(row *sqlite3.Stmt) error {
		h := sha256.New()
		for i := range 2 * len(all) {
			var b []byte
			switch row.ColumnType(i) {
			case sqlite3.INTEGER:
				b = binary.BigEndian.AppendUint64([]byte{'i'}, uint64(row.ColumnInt64(i))) //nolint:gosec // bits, not a quantity
			case sqlite3.FLOAT:
				b = binary.BigEndian.AppendUint64([]byte{'f'}, math.Float64bits(row.ColumnFloat(i)))
			case sqlite3.NULL:
				b = []byte{'n'}
			default:
				v := row.ColumnBlob(i, nil)
				b = binary.BigEndian.AppendUint64([]byte{'b'}, uint64(len(v)))
				b = append(b, v...)
			}
			h.Write(b)
		}
		v := binary.BigEndian.Uint64(h.Sum(nil))
		fp.rows++
		fp.sum += v
		fp.xor ^= v
		return nil
	})
	return fp, err
}

// verify checks that the copy holds exactly the original's values, uses only
// the planned collations, passes integrity_check, and broke no foreign keys.
func verify(src, dst *sqlite3.Conn, p Plan, tables []string) error {
	var problems []string
	for _, t := range tables {
		a, err := tableFingerprint(src, t)
		if err != nil {
			return err
		}
		b, err := tableFingerprint(dst, t)
		if err != nil {
			return err
		}
		if a != b {
			problems = append(problems, fmt.Sprintf("table %s doesn't hold exactly the original's rows (%d rows, against %d)", t, b.rows, a.rows))
		}
	}
	r, err := Find(dst)
	if err != nil {
		return err
	}
	for _, u := range r.Uses {
		problems = append(problems, "it still uses "+u.String())
	}
	for _, c := range p.Changes {
		if c.Type != "table" || c.Column == "" {
			continue
		}
		_, coll, _, _, _, err := dst.TableColumnMetadata("main", c.Object, c.Column)
		if err != nil {
			return err
		}
		if sqltext.Fold(coll) != c.To {
			problems = append(problems, fmt.Sprintf("column %s.%s has collation %s, not %s", c.Object, c.Column, coll, c.To))
		}
	}
	err = sqlite.Query(dst, `PRAGMA integrity_check`, func(st *sqlite3.Stmt) error {
		if msg := st.ColumnText(0); msg != "ok" {
			problems = append(problems, msg)
		}
		return nil
	})
	if err != nil {
		return err
	}
	broken, err := brokenKeys(src, dst)
	if err != nil {
		return err
	}
	problems = append(problems, broken...)
	if len(problems) > 0 {
		return fmt.Errorf("the converted database failed its checks, so it wasn't kept: %s", strings.Join(problems, "; "))
	}
	return nil
}

// brokenKeys compares foreign-key failures table by table. The plan has ruled
// out new ones where a parent key's collation changes, so any extra is a fault.
func brokenKeys(src, dst *sqlite3.Conn) ([]string, error) {
	count := func(c *sqlite3.Conn) (map[string]int, error) {
		n := make(map[string]int)
		err := sqlite.Query(c, `PRAGMA foreign_key_check`, func(st *sqlite3.Stmt) error { n[st.ColumnText(0)+" → "+st.ColumnText(2)]++; return nil })
		return n, err
	}
	after, err := count(dst)
	if err != nil || len(after) == 0 {
		return nil, err
	}
	before, err := count(src)
	if err != nil {
		before = nil // the original can't be checked here; any failure counts
	}
	var problems []string
	for k, n := range after {
		if n > before[k] {
			problems = append(problems, fmt.Sprintf("%d rows of %s point to missing rows through a foreign key, against %d before", n, k, before[k]))
		}
	}
	slices.Sort(problems)
	return problems, nil
}

func removeSide(path string) error {
	var errs []error
	for _, s := range []string{"-journal", "-wal", "-shm"} {
		errs = append(errs, removeIfThere(path+s))
	}
	return errors.Join(errs...)
}
