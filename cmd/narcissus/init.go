package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kistdb/narcissus/internal/index"
	"github.com/kistdb/narcissus/internal/sqlite"
	"github.com/kistdb/narcissus/internal/sqltext"
)

const queriesStarter = `-- The queries narcissus check runs, each named in a comment. For example:
--
-- -- name: recent_orders
-- SELECT * FROM orders WHERE customer_id = ? ORDER BY created_at DESC LIMIT 20;
--
-- Allow a rule for one query with a comment above it:
--
-- -- narcissus: allow full-scan
`

func runInit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := newFlagSet("init", stderr)
	dir := flags.String("dir", ".", "the `folder` to write schema.sql, indexes.sql and queries.sql to")
	force := flags.Bool("force", false, "overwrite schema.sql and indexes.sql")
	if err := parse(flags, args, 1); err != nil {
		return err
	}
	db := flags.Arg(0)
	if fi, err := os.Stat(*dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("-dir %s isn't a folder", *dir)
	}

	dbInfo, err := os.Stat(db)
	if err != nil {
		return err
	}
	c, err := sqlite.Open(ctx, db, sqlite.Options{BusyTimeout: sqlite.DefaultBusyTimeout})
	if err != nil {
		return fmt.Errorf("%s: %w", db, err)
	}
	defer c.Close()
	objs, err := sqlite.Schema(c)
	if err != nil {
		return fmt.Errorf("%s: %w", db, err)
	}
	ixs, err := sqlite.Indexes(c)
	if err != nil {
		return fmt.Errorf("%s: %w", db, err)
	}
	slices.SortFunc(ixs, index.Compare)

	header := "-- Written by narcissus init from " + clean(filepath.Base(db)) + ".\n\n"
	var schema, indexes strings.Builder
	schema.WriteString(header)
	for i, o := range objs {
		if i > 0 {
			schema.WriteString("\n")
		}
		schema.WriteString(terminate(o.SQL))
	}
	indexes.WriteString(header)
	for _, ix := range ixs {
		indexes.WriteString(terminate(ix.SQL))
	}

	files := []struct {
		name, content string
		keep          bool // leave an existing file alone, even with -force
	}{
		{"schema.sql", schema.String(), false},
		{"indexes.sql", indexes.String(), false},
		{"queries.sql", queriesStarter, true},
	}
	var writes []pendingWrite
	for _, f := range files {
		path := filepath.Join(*dir, f.name)
		fi, err := os.Lstat(path) // a dangling symlink counts as existing
		if err == nil && os.SameFile(fi, dbInfo) {
			return fmt.Errorf("%s is the database itself; please choose another -dir", path)
		}
		switch {
		case err == nil && fi.IsDir():
			return fmt.Errorf("%s is a folder", path)
		case err == nil && f.keep:
			fmt.Fprintf(stdout, "kept %s\n", path)
			continue
		case err == nil && !*force:
			return fmt.Errorf("%s already exists (-force overwrites it)", path)
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return err
		}
		w := pendingWrite{path: path, content: f.content}
		if err == nil {
			w.mode = fi.Mode().Perm() // keep the permissions of the file -force replaces
		}
		writes = append(writes, w)
	}
	if err := writeAll(*dir, writes); err != nil {
		return err
	}
	for _, w := range writes {
		fmt.Fprintf(stdout, "wrote %s\n", w.path)
	}
	fmt.Fprintf(stdout, "%d tables, views and triggers; %d %s. Next: add your queries to queries.sql and run narcissus check.\n",
		len(objs), len(ixs), plural(len(ixs), "index", "indexes"))
	return nil
}

// terminate ends a stored statement with a semicolon, after closing a
// trailing comment SQLite let run to the end, or on its own line after a "--" one.
func terminate(sql string) string {
	toks, err := sqltext.Lex(sql)
	switch {
	case errors.Is(err, sqltext.ErrUnterminatedComment):
		return sql + " */;\n"
	case err == nil && len(toks) > 0 && strings.Contains(sql[toks[len(toks)-1].End:], "--"):
		return sql + "\n;\n"
	default:
		return sql + ";\n"
	}
}

type pendingWrite struct {
	path, content string
	mode          fs.FileMode // zero for a new file, which gets the umask's
}

// writeAll writes every file to a temporary name in dir, then renames them
// into place, so a failure leaves the old files untouched.
func writeAll(dir string, writes []pendingWrite) (err error) {
	var temps []string
	defer func() {
		for _, t := range temps {
			if t != "" {
				os.Remove(t)
			}
		}
	}()
	for _, w := range writes {
		f, err := createTemp(dir)
		if err != nil {
			return err
		}
		temps = append(temps, f.Name())
		_, err = f.WriteString(w.content)
		if w.mode != 0 {
			err = errors.Join(err, f.Chmod(w.mode))
		}
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
	}
	for i, w := range writes {
		if err := os.Rename(temps[i], w.path); err != nil {
			return err
		}
		temps[i] = ""
	}
	return nil
}

// createTemp creates a new file in dir with the umask's permissions, as any
// file you'd write yourself; os.CreateTemp would make it private.
func createTemp(dir string) (*os.File, error) {
	for {
		name := filepath.Join(dir, ".narcissus-"+rand.Text()[:12]+".sql")
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666) //nolint:gosec // source files, readable like the rest of the repo
		if !errors.Is(err, fs.ErrExist) {
			return f, err
		}
	}
}
