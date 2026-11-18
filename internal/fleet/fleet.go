// Package fleet plans and applies index changes across many databases.
package fleet

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ncruces/go-sqlite3"

	"github.com/kistdb/narcissus/internal/index"
	"github.com/kistdb/narcissus/internal/sqlite"
	"github.com/kistdb/narcissus/internal/sqltext"
)

// Options control a run.
type Options struct {
	Prune             index.Prune
	Apply             bool // make the changes, not just plan them
	AllowAppDependent bool // allow indexes whose content depends on the app's collations or functions
	Jobs              int  // databases handled at once
	BusyTimeout       time.Duration
}

// Result is the outcome for one database.
type Result struct {
	Path string
	Plan index.Plan
	Err  error
}

// Expand returns the database files args name, each once however many paths
// lead to it, and the glob matches it skipped because they aren't regular files.
func Expand(args []string) (paths, skipped []string, err error) {
	var seen []os.FileInfo
	add := func(path string, fi os.FileInfo) {
		if !slices.ContainsFunc(seen, func(s os.FileInfo) bool { return os.SameFile(s, fi) }) {
			seen = append(seen, fi)
			paths = append(paths, path)
		}
	}
	for _, arg := range args {
		if fi, err := os.Stat(arg); err == nil || !strings.ContainsAny(arg, "*?[") {
			if err != nil {
				return nil, nil, err
			}
			if !fi.Mode().IsRegular() {
				return nil, nil, fmt.Errorf("%s is not a regular file", arg)
			}
			add(arg, fi)
			continue
		}
		matches, err := glob(arg)
		if err != nil {
			return nil, nil, err
		}
		found := false
		for _, m := range matches {
			if strings.HasSuffix(m, "-wal") || strings.HasSuffix(m, "-shm") || strings.HasSuffix(m, "-journal") {
				continue
			}
			fi, err := os.Stat(m)
			if err != nil || !fi.Mode().IsRegular() {
				skipped = append(skipped, m)
				continue
			}
			add(m, fi)
			found = true
		}
		if !found {
			return nil, nil, fmt.Errorf("no databases match %s", arg)
		}
	}
	return paths, skipped, nil
}

// glob is filepath.Glob, except that a folder it can't read is an error, not a silent gap.
func glob(pattern string) ([]string, error) {
	if _, err := filepath.Match(pattern, ""); err != nil {
		return nil, fmt.Errorf("%s: %w", pattern, err)
	}
	dir, file := filepath.Split(pattern)
	dir = cleanDir(dir)
	if !strings.ContainsAny(dir, "*?[") {
		return matchDir(dir, file)
	}
	dirs, err := glob(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			continue
		}
		m, err := matchDir(d, file)
		if err != nil {
			return nil, err
		}
		out = append(out, m...)
	}
	return out, nil
}

func matchDir(dir, pattern string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if ok, _ := filepath.Match(pattern, e.Name()); ok {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

// cleanDir prepares the folder part of a pattern, as filepath.Glob does.
func cleanDir(dir string) string {
	switch {
	case dir == "":
		return "."
	case dir == string(filepath.Separator):
		return dir
	case filepath.VolumeName(dir)+string(filepath.Separator) == dir:
		return dir
	default:
		return dir[:len(dir)-1]
	}
}

// Run plans, and with opts.Apply makes, each database's changes, opts.Jobs at
// a time. It calls report, if set, with each result in the order of paths.
func Run(ctx context.Context, paths []string, desired []index.Index, opts Options, report func(Result)) []Result {
	results := make([]Result, len(paths))
	done := make([]chan struct{}, len(paths))
	for i := range done {
		done[i] = make(chan struct{})
	}
	sem := make(chan struct{}, max(opts.Jobs, 1))
	var wg sync.WaitGroup
	wg.Go(func() {
		for i, path := range paths {
			results[i].Path = path
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i].Err = ctx.Err()
				close(done[i])
				continue
			}
			wg.Go(func() {
				defer func() { <-sem; close(done[i]) }()
				results[i].Plan, results[i].Err = one(ctx, path, desired, opts)
			})
		}
	})
	for i := range paths {
		<-done[i]
		if report != nil {
			report(results[i])
		}
	}
	wg.Wait()
	return results
}

// one plans, and maybe applies, the changes for the database at path. It
// takes the write lock only when there is something to change.
func one(ctx context.Context, path string, desired []index.Index, opts Options) (_ index.Plan, err error) {
	c, err := sqlite.Open(ctx, path, sqlite.Options{Write: opts.Apply, BusyTimeout: opts.BusyTimeout})
	if err != nil {
		return index.Plan{}, err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	plan, err := diff(c, desired, opts.Prune)
	if err != nil || !opts.Apply || plan.Empty() {
		return plan, err
	}

	var tx sqlite3.Txn
	if tx, err = c.BeginImmediate(); err != nil {
		return index.Plan{}, err
	}
	defer tx.End(&err)
	if plan, err = diff(c, desired, opts.Prune); err != nil { // again, under the lock
		return index.Plan{}, err
	}
	for _, ch := range plan.Changes {
		if err := change(c, ch, opts); err != nil {
			return index.Plan{}, fmt.Errorf("%s: %w", ch.Index.Name, err)
		}
	}
	return plan, nil
}

// change makes one change. It refuses to drop a UNIQUE index's constraint
// without PruneAll, or to build an index that depends on the app unless allowed.
func change(c *sqlite3.Conn, ch index.Change, opts Options) error {
	if ch.Action == index.Replace && ch.Old.Unique && !ch.Index.Unique && opts.Prune != index.PruneAll {
		return errors.New("replacing a UNIQUE index with one that isn't drops its constraint; pass -prune -prune-unique to allow it")
	}
	if ch.Action != index.Create {
		if err := sqlite.ExecIndexChange(c, `DROP INDEX `+sqltext.Quote(ch.Index.Name), nil); err != nil {
			return err
		}
	}
	if ch.Action == index.Drop {
		return nil
	}
	var vet func(sqlite.Reads) error
	if !opts.AllowAppDependent {
		vet = func(r sqlite.Reads) error { return vetAppDependent(c, ch.Index, r) }
	}
	return sqlite.ExecIndexChange(c, ch.Index.SQL, vet)
}

// vetAppDependent refuses an index whose content depends on how the app
// defines collations or functions: SQLite's reads show every function, operator and column.
func vetAppDependent(c *sqlite3.Conn, ix index.Index, r sqlite.Reads) error {
	var deps []string
	for _, d := range ix.AppDependent() {
		if strings.HasPrefix(d, "COLLATE ") {
			deps = append(deps, d)
		}
	}
	for _, f := range r.Functions {
		deps = append(deps, f+"()")
	}
	for _, tc := range r.Columns {
		_, coll, _, _, _, err := c.TableColumnMetadata("main", tc[0], tc[1])
		if err != nil {
			return fmt.Errorf("column %s.%s: %w", tc[0], tc[1], err)
		}
		if !strings.EqualFold(coll, "BINARY") {
			deps = append(deps, fmt.Sprintf("column %s with COLLATE %s", tc[1], coll))
		}
		virtual := false
		err = sqlite.Query(c, `SELECT 1 FROM pragma_table_xinfo(`+sqltext.QuoteString(tc[0])+`) WHERE name = `+sqltext.QuoteString(tc[1])+` AND hidden = 2`,
			func(*sqlite3.Stmt) error { virtual = true; return nil })
		if err != nil {
			return err
		}
		if virtual {
			deps = append(deps, fmt.Sprintf("generated column %s", tc[1]))
		}
	}
	if len(deps) == 0 {
		return nil
	}
	slices.Sort(deps)
	return fmt.Errorf("uses %s, which an app can define differently, leaving it a corrupt index; "+
		"pass -allow-collations-and-functions if your app uses SQLite's built-ins", strings.Join(slices.Compact(deps), ", "))
}

func diff(c *sqlite3.Conn, desired []index.Index, prune index.Prune) (index.Plan, error) {
	actual, err := sqlite.Indexes(c)
	if err != nil {
		return index.Plan{}, err
	}
	return index.Diff(desired, actual, prune), nil
}
