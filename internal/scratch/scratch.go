// Package scratch builds an in-memory copy of a schema with the desired
// indexes, for checking query plans.
package scratch

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/ncruces/go-sqlite3"

	"github.com/kistdb/narcissus/internal/index"
	"github.com/kistdb/narcissus/internal/sqlite"
	"github.com/kistdb/narcissus/internal/sqltext"
)

// DB is a scratch database.
type DB struct {
	Conn    *sqlite3.Conn
	Indexes []index.Index // the desired indexes
	Ignored []string      // indexes the schema made that aren't desired
}

// Close closes the database.
func (d *DB) Close() error { return d.Conn.Close() }

// Build loads the schema from schemaPath (a SQL file, database file or
// migrations directory), then replaces its indexes with those in indexesPath.
func Build(ctx context.Context, schemaPath, indexesPath string) (_ *DB, err error) {
	src, err := os.ReadFile(indexesPath)
	if err != nil {
		return nil, err
	}
	desired, err := index.Parse(indexesPath, string(src))
	if err != nil {
		return nil, err
	}

	c, err := sqlite.OpenMemory(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			c.Close()
		}
	}()
	var unbuilt []string
	isDB, err := loadSchema(ctx, c, schemaPath, &unbuilt)
	if err != nil {
		return nil, err
	}
	d := &DB{Conn: c, Indexes: desired}
	if d.Ignored, err = dropIndexes(c, desired); err != nil {
		return nil, err
	}
	for _, name := range unbuilt {
		if !slices.ContainsFunc(desired, func(ix index.Index) bool { return sqltext.Fold(ix.Name) == sqltext.Fold(name) }) {
			d.Ignored = append(d.Ignored, name)
		}
	}
	// Indexes are built as apply builds them: "x" must name a column.
	if err := sqlite.AllowDoubleQuotedStrings(c, false); err != nil {
		return nil, err
	}
	if err := (sqlite.Script{File: indexesPath, Line: 1, Text: string(src)}).Run(c); err != nil {
		return nil, err
	}
	if isDB {
		if err := copyStats(ctx, c, schemaPath); err != nil {
			return nil, fmt.Errorf("%s: copy statistics: %w", schemaPath, err)
		}
	}
	// Queries are run as the app's SQLite would, with its defaults.
	if err := sqlite.AllowDoubleQuotedStrings(c, true); err != nil {
		return nil, err
	}
	return d, nil
}

// loadSchema runs the schema at path into c, reporting whether it came from a database file.
func loadSchema(ctx context.Context, c *sqlite3.Conn, path string, unbuilt *[]string) (isDB bool, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if fi.IsDir() {
		scripts, err := migrations(path)
		if err != nil {
			return false, err
		}
		for _, s := range scripts {
			if err := s.Run(c); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file or folder", path)
	}
	isDB, err = sqlite.IsDatabase(path)
	if err != nil {
		return false, err
	}
	if isDB {
		return true, loadDatabase(ctx, c, path, unbuilt)
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return false, sqlite.Script{File: path, Line: 1, Text: string(text)}.Run(c)
}

// loadDatabase copies the schema, indexes included, of the database at path into c.
func loadDatabase(ctx context.Context, c *sqlite3.Conn, path string, unbuilt *[]string) error {
	src, err := sqlite.Open(ctx, path, sqlite.Options{BusyTimeout: sqlite.DefaultBusyTimeout})
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer src.Close()
	objs, err := sqlite.Schema(src)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, o := range objs {
		if err := c.Exec(o.SQL); err != nil {
			return fmt.Errorf("%s: %s %s: %w", path, o.Type, o.Name, err)
		}
	}
	ixs, err := sqlite.Indexes(src)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, ix := range ixs {
		// One the scratch database can't build, as with an app's own collation, is left out.
		if c.Exec(ix.SQL) != nil {
			*unbuilt = append(*unbuilt, ix.Name)
		}
	}
	return nil
}

// dropIndexes drops the indexes the schema made, returning the names of those not desired.
func dropIndexes(c *sqlite3.Conn, desired []index.Index) ([]string, error) {
	have, err := sqlite.Indexes(c)
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(desired))
	for _, ix := range desired {
		want[sqltext.Fold(ix.Name)] = true
	}
	var ignored []string
	for _, ix := range have {
		if err := c.Exec(`DROP INDEX ` + sqltext.Quote(ix.Name)); err != nil {
			return nil, err
		}
		if !want[sqltext.Fold(ix.Name)] {
			ignored = append(ignored, ix.Name)
		}
	}
	return ignored, nil
}

// copyStats copies the planner statistics of the database at path into c.
func copyStats(ctx context.Context, c *sqlite3.Conn, path string) error {
	src, err := sqlite.Open(ctx, path, sqlite.Options{BusyTimeout: sqlite.DefaultBusyTimeout})
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer src.Close()
	// ANALYZE creates the statistics tables, which can't be created directly.
	if err := c.Exec(`ANALYZE`); err != nil {
		return err
	}
	for _, table := range []string{"sqlite_stat1", "sqlite_stat4"} {
		if err := copyTable(src, c, table); err != nil {
			return err
		}
	}
	return c.Exec(`ANALYZE sqlite_schema`) // reload the statistics
}

func copyTable(src, dst *sqlite3.Conn, table string) error {
	exists := false
	err := sqlite.Query(src, `SELECT 1 FROM sqlite_schema WHERE name = '`+table+`'`, func(*sqlite3.Stmt) error {
		exists = true
		return nil
	})
	if err != nil || !exists {
		return err
	}
	if err := dst.Exec(`DELETE FROM ` + table); err != nil {
		return err
	}
	var ins *sqlite3.Stmt
	defer func() {
		if ins != nil {
			ins.Close()
		}
	}()
	return sqlite.Query(src, `SELECT * FROM `+table, func(row *sqlite3.Stmt) error {
		if ins == nil {
			params := strings.Repeat(", ?", row.ColumnCount())[2:]
			if ins, _, err = dst.Prepare(`INSERT INTO ` + table + ` VALUES (` + params + `)`); err != nil {
				return err
			}
		}
		for i := range row.ColumnCount() {
			if err := sqlite.BindColumn(ins, i+1, row, i); err != nil {
				return err
			}
		}
		return ins.Exec()
	})
}
