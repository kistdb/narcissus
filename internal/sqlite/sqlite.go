// Package sqlite opens SQLite databases and reads and runs their SQL.
package sqlite

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/ext/fts5"
	"github.com/ncruces/go-sqlite3/ext/rtree"

	"github.com/kistdb/narcissus/internal/index"
	"github.com/kistdb/narcissus/internal/sqltext"
)

func init() {
	sqlite3.AutoExtension(fts5.Register)
	sqlite3.AutoExtension(rtree.Register)
}

// OpenMemory opens an empty in-memory database that ctx can interrupt.
func OpenMemory(ctx context.Context) (*sqlite3.Conn, error) {
	return open(ctx, ":memory:", sqlite3.OPEN_READWRITE|sqlite3.OPEN_CREATE|sqlite3.OPEN_MEMORY)
}

// Open opens an existing database file, read-only unless write is set.
func Open(ctx context.Context, path string, write bool) (*sqlite3.Conn, error) {
	if write {
		return open(ctx, path, sqlite3.OPEN_READWRITE)
	}
	c, err := open(ctx, path, sqlite3.OPEN_READONLY)
	if errors.Is(err, sqlite3.READONLY) || errors.Is(err, sqlite3.CANTOPEN) {
		// A WAL database in a read-only folder; nothing here can be writing it.
		uri := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: "immutable=1"}
		if c, ierr := open(ctx, uri.String(), sqlite3.OPEN_READONLY|sqlite3.OPEN_URI); ierr == nil {
			return c, nil
		}
	}
	return c, err
}

// open opens a connection with stock SQLite's defaults, which the library
// changes: double-quoted strings allowed, foreign keys off.
func open(ctx context.Context, path string, flags sqlite3.OpenFlag) (*sqlite3.Conn, error) {
	c, err := sqlite3.OpenFlags(path, flags)
	if err != nil {
		return nil, err
	}
	c.SetInterrupt(ctx)
	for _, op := range []sqlite3.DBConfig{sqlite3.DBCONFIG_DQS_DDL, sqlite3.DBCONFIG_DQS_DML} {
		if _, err := c.Config(op, true); err != nil {
			return nil, errors.Join(err, c.Close())
		}
	}
	if err := c.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
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
	const q = `SELECT name, tbl_name, sql FROM sqlite_schema WHERE type = 'index' AND sql IS NOT NULL`
	var ixs []index.Index
	err := Query(c, q, func(st *sqlite3.Stmt) error {
		ix, err := index.FromSQL(st.ColumnText(2))
		if err != nil {
			ix = index.Raw(st.ColumnText(0), st.ColumnText(1), st.ColumnText(2))
		}
		ixs = append(ixs, ix)
		return nil
	})
	return ixs, err
}
