package main

import (
	"context"
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

	c, err := sqlite.Open(ctx, db, false)
	if err != nil {
		return err
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

	header := "-- Written by narcissus init from " + filepath.Base(db) + ".\n\n"
	var schema, indexes strings.Builder
	schema.WriteString(header)
	for _, o := range objs {
		schema.WriteString(o.SQL + ";\n\n")
	}
	indexes.WriteString(header)
	for _, ix := range ixs {
		indexes.WriteString(ix.SQL + ";\n")
	}

	files := []struct {
		name, content string
		keep          bool // leave an existing file alone, even with -force
	}{
		{"schema.sql", strings.TrimRight(schema.String(), "\n") + "\n", false},
		{"indexes.sql", indexes.String(), false},
		{"queries.sql", queriesStarter, true},
	}
	for _, f := range files {
		path := filepath.Join(*dir, f.name)
		if _, err := os.Stat(path); err == nil && !*force && !f.keep {
			return fmt.Errorf("%s already exists (-force overwrites it)", path)
		}
	}
	for _, f := range files {
		path := filepath.Join(*dir, f.name)
		mode := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if f.keep {
			mode = os.O_WRONLY | os.O_CREATE | os.O_EXCL
		}
		switch err := writeFile(path, mode, f.content); {
		case errors.Is(err, fs.ErrExist):
			fmt.Fprintf(stdout, "kept %s\n", path)
		case err != nil:
			return err
		default:
			fmt.Fprintf(stdout, "wrote %s\n", path)
		}
	}
	fmt.Fprintf(stdout, "%d tables, views and triggers; %d %s. Next: add your queries to queries.sql and run narcissus check.\n",
		len(objs), len(ixs), plural(len(ixs), "index", "indexes"))
	return nil
}

func writeFile(path string, flags int, content string) error {
	f, err := os.OpenFile(path, flags, 0o644) //nolint:gosec // source files, readable like the rest of the repo
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	return errors.Join(err, f.Close())
}
