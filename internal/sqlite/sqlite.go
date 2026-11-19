// Package sqlite opens SQLite databases and reads and runs their SQL.
package sqlite

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/ext/fts5"
	"github.com/ncruces/go-sqlite3/ext/rtree"
	"github.com/ncruces/go-sqlite3/vfs"

	"github.com/kistdb/narcissus/internal/index"
	"github.com/kistdb/narcissus/internal/sqltext"
)

func init() {
	sqlite3.AutoExtension(fts5.Register)
	sqlite3.AutoExtension(rtree.Register)
}

// DefaultBusyTimeout is how long commands wait for a locked database unless told otherwise.
const DefaultBusyTimeout = 5 * time.Second

// OpenMemory opens an empty in-memory database that ctx can interrupt.
func OpenMemory(ctx context.Context) (*sqlite3.Conn, error) {
	return open(ctx, ":memory:", sqlite3.OPEN_READWRITE|sqlite3.OPEN_CREATE|sqlite3.OPEN_MEMORY, 0)
}

// Options control how Open opens a database file.
type Options struct {
	Write       bool
	BusyTimeout time.Duration
}

// Open opens an existing SQLite database file that has one name. For writing,
// it also needs a supported platform. Its errors don't repeat the path.
func Open(ctx context.Context, path string, opts Options) (*sqlite3.Conn, error) {
	abs, err := filepath.Abs(path)
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs) // side files sit beside the real file
	}
	if err != nil {
		return nil, err
	}
	if len(abs) > maxPath {
		return nil, fmt.Errorf("path longer than %d bytes", maxPath)
	}
	fi, err := checkFile(abs)
	if err != nil {
		return nil, err
	}
	n, err := links(abs, fi)
	if err != nil {
		return nil, err
	}
	if n > 1 {
		return nil, fmt.Errorf("the file has %d names (hard links), and SQLite's journal follows the name, so it can't be used safely", n)
	}
	busy := min(opts.BusyTimeout, math.MaxInt32*time.Millisecond)
	q := url.Values{
		"_pragma": {fmt.Sprintf("busy_timeout(%d)", busy.Milliseconds())}, // before the library's own setup reads the file
		"modeof":  {abs},                                                  // a journal or WAL gets the database's permissions
	}
	flags := sqlite3.OPEN_URI | sqlite3.OPEN_READWRITE
	if opts.Write {
		if !writeSupported {
			return nil, fmt.Errorf("changing databases on %s isn't supported yet", runtime.GOOS)
		}
	} else {
		q.Set("mode", "ro")
		flags = sqlite3.OPEN_URI | sqlite3.OPEN_READONLY
	}
	c, err := openFile(ctx, uri(abs, q), flags, busy)
	immutable := false
	if !opts.Write && (errors.Is(err, sqlite3.READONLY) || errors.Is(err, sqlite3.CANTOPEN)) && complete(abs) {
		// A read-only folder, and no journal or WAL content that immutable would miss.
		q.Set("immutable", "1")
		if ic, ierr := openFile(ctx, uri(abs, q), flags, busy); ierr == nil {
			c, err, immutable = ic, nil, true
		}
	}
	if err != nil {
		if errors.Is(err, sqlite3.READONLY) && exists(abs+"-journal") {
			err = fmt.Errorf("%w (an interrupted write left a journal; run apply, or open the database once with write access, to recover it)", err)
		}
		return nil, err
	}
	if err := verify(c, abs, fi, immutable); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	matchShm(abs, fi)
	return c, nil
}

// openGrace is how long opening may take beyond the busy timeout.
const openGrace = 30 * time.Second

// openFile opens a connection to a file, giving up on Ctrl-C or after the
// busy timeout and a grace period, as when the file was swapped for a FIFO.
func openFile(ctx context.Context, name string, flags sqlite3.OpenFlag, busy time.Duration) (*sqlite3.Conn, error) {
	type result struct {
		c   *sqlite3.Conn
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := open(ctx, name, flags, busy)
		done <- result{c, err}
	}()
	timer := time.NewTimer(busy + openGrace)
	defer timer.Stop()
	var err error
	select {
	case r := <-done:
		return r.c, r.err
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = errors.New("timed out opening the database")
	}
	go func() { // close what a late open returns
		if r := <-done; r.c != nil {
			r.c.Close()
		}
	}()
	return nil, err
}

// verify checks that the file SQLite opened is the regular, singly linked file
// Open checked, closing the gap in which it could have been swapped.
func verify(c *sqlite3.Conn, abs string, want fs.FileInfo, immutable bool) error {
	f, err := c.FileControl("main", sqlite3.FCNTL_FILE_POINTER)
	if err != nil {
		return err
	}
	type statter interface{ Stat() (fs.FileInfo, error) }
	st, ok := f.(statter)
	for !ok { // the library may wrap the OS file
		u, wrapped := f.(vfs.FileUnwrap)
		if !wrapped {
			return errors.New("can't check the file SQLite opened")
		}
		f = u.Unwrap()
		st, ok = f.(statter)
	}
	fi, err := st.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(fi, want) {
		return errors.New("the file changed while it was being opened")
	}
	n, err := links(abs, fi)
	if err != nil {
		return err
	}
	if n > 1 {
		return errors.New("the file gained a second name while it was being opened")
	}
	if immutable && !complete(abs) {
		return errors.New("a journal or WAL appeared while the read-only snapshot was being opened")
	}
	return nil
}

// maxPath keeps paths within what the SQLite library accepts.
const maxPath = 900

// open opens a connection that can't attach other files, with foreign keys off
// as in stock SQLite; double-quoted strings are on only for in-memory databases.
func open(ctx context.Context, name string, flags sqlite3.OpenFlag, busy time.Duration) (*sqlite3.Conn, error) {
	c, err := sqlite3.OpenFlags(name, flags)
	if err != nil {
		return nil, err
	}
	if err := setup(ctx, c, flags, busy); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

func setup(ctx context.Context, c *sqlite3.Conn, flags sqlite3.OpenFlag, busy time.Duration) error {
	if err := c.BusyTimeout(busy); err != nil {
		return err
	}
	c.SetInterrupt(ctx)
	c.Limit(sqlite3.LIMIT_ATTACHED, 0) // no ATTACH or VACUUM INTO
	// A stale "column" must fail, not become a string, in what we run on a database file.
	if err := AllowDoubleQuotedStrings(c, flags&sqlite3.OPEN_MEMORY != 0); err != nil {
		return err
	}
	pragmas := `PRAGMA foreign_keys = OFF`
	if flags&sqlite3.OPEN_READWRITE != 0 && flags&sqlite3.OPEN_MEMORY == 0 {
		pragmas += `; PRAGMA synchronous = FULL`
	}
	return c.Exec(pragmas)
}

// AllowDoubleQuotedStrings sets whether "x" may mean a string when no column x
// exists, as stock SQLite allows for the schemas and scripts it runs.
func AllowDoubleQuotedStrings(c *sqlite3.Conn, on bool) error {
	for _, op := range []sqlite3.DBConfig{sqlite3.DBCONFIG_DQS_DDL, sqlite3.DBCONFIG_DQS_DML} {
		if _, err := c.Config(op, on); err != nil {
			return err
		}
	}
	return nil
}

// uri returns a file: URI for abs, as file:/dir/x.db or file:C:/dir/x.db.
func uri(abs string, q url.Values) string {
	path := (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath()
	// SQLite decodes %XX but not "+", which Encode uses for spaces.
	return "file:" + path + "?" + strings.ReplaceAll(q.Encode(), "+", "%20")
}

// IsDatabase reports whether path is a regular file that starts with the SQLite header.
func IsDatabase(path string) (bool, error) {
	_, err := checkFile(path)
	if errors.Is(err, errNotDatabase) {
		return false, nil
	}
	return err == nil, err
}

var errNotDatabase = errors.New("not a SQLite database")

// checkFile opens path without blocking or following a final symlink, and
// checks through that one handle that it's a regular file with the SQLite header.
func checkFile(path string) (fs.FileInfo, error) {
	f, err := openNoBlock(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotDatabase
	}
	header := make([]byte, 100)
	if _, err := io.ReadFull(f, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errNotDatabase
		}
		return nil, err
	}
	if string(header[:16]) != "SQLite format 3\x00" {
		return nil, errNotDatabase
	}
	if header[20] == 8 { // the library would treat it as checksummed, and write WAL frames wrongly
		return nil, errors.New("it reserves 8 bytes per page, as SQLite's checksum format does, which Narcissus doesn't support yet")
	}
	return fi, nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// complete reports whether the database file holds all its content: no
// rollback journal, and no WAL file with frames in it.
func complete(abs string) bool {
	if exists(abs + "-journal") {
		return false
	}
	fi, err := os.Lstat(abs + "-wal")
	return errors.Is(err, fs.ErrNotExist) || err == nil && fi.Size() == 0
}

// Reads lists what a statement uses, as SQLite's authorizer reports it.
type Reads struct {
	Functions []string    // functions, including those behind operators such as LIKE and ->>
	Columns   [][2]string // table and column pairs
}

// ExecIndexChange runs sql, one CREATE INDEX or DROP INDEX, under an authorizer
// that refuses anything else. A vet sees what it reads first, and can refuse it.
func ExecIndexChange(c *sqlite3.Conn, sql string, vet func(Reads) error) error {
	return indexChange(c, sql, vet, true)
}

// PrepareIndexChange checks sql as ExecIndexChange does, without running it.
func PrepareIndexChange(c *sqlite3.Conn, sql string, vet func(Reads) error) error {
	return indexChange(c, sql, vet, false)
}

func indexChange(c *sqlite3.Conn, sql string, vet func(Reads) error, run bool) (err error) {
	var r Reads
	vetting := false // vet's own queries aren't the change
	err = c.SetAuthorizer(func(action sqlite3.AuthorizerActionCode, name3, name4, schema, _ string) sqlite3.AuthorizerReturnCode {
		if vetting {
			return sqlite3.AUTH_OK
		}
		switch action {
		case sqlite3.AUTH_FUNCTION:
			r.Functions = append(r.Functions, name4)
			return sqlite3.AUTH_OK
		case sqlite3.AUTH_READ:
			if name3 != "sqlite_master" {
				r.Columns = append(r.Columns, [2]string{name3, name4})
			}
			return sqlite3.AUTH_OK
		case sqlite3.AUTH_REINDEX:
			return sqlite3.AUTH_OK
		case sqlite3.AUTH_CREATE_INDEX, sqlite3.AUTH_DROP_INDEX:
			if schema == "main" {
				return sqlite3.AUTH_OK
			}
		case sqlite3.AUTH_INSERT, sqlite3.AUTH_UPDATE:
			if schema == "main" && name3 == "sqlite_master" {
				return sqlite3.AUTH_OK
			}
		case sqlite3.AUTH_DELETE: // DROP INDEX also removes the index's statistics
			if schema == "main" && slices.Contains([]string{"sqlite_master", "sqlite_stat1", "sqlite_stat2", "sqlite_stat3", "sqlite_stat4"}, name3) {
				return sqlite3.AUTH_OK
			}
		}
		return sqlite3.AUTH_DENY
	})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.SetAuthorizer(nil)) }()
	st, tail, err := c.Prepare(sql)
	if err != nil {
		return err
	}
	defer st.Close()
	if st == nil || sqltext.SkipSpace(tail) != len(tail) {
		return fmt.Errorf("want exactly one statement: %s", sql)
	}
	if vet != nil {
		vetting = true
		err := vet(r)
		vetting = false
		if err != nil {
			return err
		}
	}
	if !run {
		return nil
	}
	return st.Exec()
}

// Query runs sql and calls fn for each row.
func Query(c *sqlite3.Conn, sql string, fn func(*sqlite3.Stmt) error) error {
	st, _, err := c.Prepare(sql)
	if err != nil {
		return err
	}
	defer st.Close()
	for st.Step() {
		if err := fn(st); err != nil {
			return err
		}
	}
	return st.Err()
}

// Script is SQL text from a file, starting at Line of that file.
type Script struct {
	File string
	Line int
	Text string
}

// Run executes the statements in turn. Where a semicolon splits a trigger
// body, SQLite reports incomplete input and the next piece is joined on.
func (s Script) Run(c *sqlite3.Conn) error {
	stmts, err := sqltext.Split(s.File, s.Text)
	if serr, ok := errors.AsType[*sqltext.Error](err); ok {
		serr.Line += s.Line - 1
	}
	if err != nil {
		return err
	}
	for i := 0; i < len(stmts); i++ {
		first := stmts[i]
		for {
			err := c.Exec(s.Text[first.Start:stmts[i].End])
			if err == nil {
				break
			}
			if strings.Contains(err.Error(), "incomplete input") && i+1 < len(stmts) {
				i++
				continue
			}
			if strings.Contains(err.Error(), "too many attached databases") {
				err = errors.New("ATTACH and VACUUM INTO can't be used here, as they could change other files")
			}
			return &sqltext.Error{File: s.File, Line: s.Line + first.Line - 1, Err: err}
		}
	}
	return nil
}

// Object is a table, view or trigger in a schema.
type Object struct {
	Type, Name, SQL string
}

// shadowSuffixes name the tables FTS3, FTS4, FTS5 and R*Tree keep for a virtual table.
var shadowSuffixes = []string{"content", "segments", "segdir", "docsize", "stat", "data", "idx", "config", "node", "parent", "rowid"}

// Schema returns the tables, views and triggers in an order that recreates
// them, leaving out indexes and SQLite's internal and shadow tables.
func Schema(c *sqlite3.Conn) ([]Object, error) {
	const q = `SELECT type, name, sql FROM sqlite_schema
		WHERE sql IS NOT NULL AND type IN ('table', 'view', 'trigger')
		AND name NOT LIKE 'sqlite\_%' ESCAPE '\'
		AND name NOT IN (SELECT name FROM pragma_table_list WHERE schema = 'main' AND type = 'shadow')
		ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'view' THEN 1 ELSE 2 END, rowid`
	var objs []Object
	err := Query(c, q, func(st *sqlite3.Stmt) error {
		objs = append(objs, Object{st.ColumnText(0), st.ColumnText(1), st.ColumnText(2)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Shadow tables of modules not loaded here aren't marked as such.
	var vtabs []string
	for _, o := range objs {
		if strings.HasPrefix(sqltext.Fold(o.SQL), "create virtual table") {
			vtabs = append(vtabs, sqltext.Fold(o.Name)+"_")
		}
	}
	return slices.DeleteFunc(objs, func(o Object) bool {
		name := sqltext.Fold(o.Name)
		for _, v := range vtabs {
			if suffix, ok := strings.CutPrefix(name, v); ok && slices.Contains(shadowSuffixes, suffix) {
				return true
			}
		}
		return false
	}), nil
}

// Indexes returns the indexes made with CREATE INDEX, leaving out those
// SQLite makes for constraints.
func Indexes(c *sqlite3.Conn) ([]index.Index, error) {
	const q = `SELECT s.name, s.tbl_name, s.sql, l."unique" FROM sqlite_schema AS s
		JOIN pragma_index_list(s.tbl_name) AS l ON l.name = s.name
		WHERE s.type = 'index' AND s.sql IS NOT NULL`
	var ixs []index.Index
	err := Query(c, q, func(st *sqlite3.Stmt) error {
		ix, err := index.FromSQL(st.ColumnText(2))
		if err != nil {
			ix = index.Raw(st.ColumnText(0), st.ColumnText(1), st.ColumnText(2))
		}
		ix.Unique = st.ColumnBool(3) // SQLite's word, not the parser's
		ixs = append(ixs, ix)
		return nil
	})
	return ixs, err
}

// FreeName returns base, or base with a number added, that no schema object has.
func FreeName(c *sqlite3.Conn, base string) (string, error) {
	taken := make(map[string]bool)
	err := Query(c, `SELECT name FROM sqlite_schema`, func(st *sqlite3.Stmt) error {
		taken[sqltext.Fold(st.ColumnText(0))] = true
		return nil
	})
	name := base
	for n := 2; taken[sqltext.Fold(name)]; n++ {
		name = fmt.Sprintf("%s_%d", base, n)
	}
	return name, err
}
