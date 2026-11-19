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
	Path    string
	Plan    index.Plan
	Refused []error // changes that would be refused; then none were made
	Err     error
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
		if strings.Contains(arg, "**") {
			return nil, nil, fmt.Errorf("%s: ** isn't supported; give each folder level, as in 'tenants/*/*.db'", arg)
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
				results[i].Plan, results[i].Refused, results[i].Err = one(ctx, path, desired, opts)
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

// one plans, and maybe applies, a database's changes. If any change would be
// refused, none is made; the write lock is taken only to change something.
func one(ctx context.Context, path string, desired []index.Index, opts Options) (_ index.Plan, refused []error, err error) {
	c, err := sqlite.Open(ctx, path, sqlite.Options{Write: opts.Apply, BusyTimeout: opts.BusyTimeout})
	if err != nil {
		return index.Plan{}, nil, err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	v, err := newVetter(c, opts.AllowAppDependent)
	if err != nil {
		return index.Plan{}, nil, err
	}
	plan, err := diff(c, desired, opts.Prune)
	if err != nil || plan.Empty() {
		return plan, nil, err
	}
	if refused = check(c, v, plan, opts.Prune); len(refused) > 0 || !opts.Apply {
		return plan, refused, nil
	}

	var tx sqlite3.Txn
	if tx, err = c.BeginImmediate(); err != nil {
		return index.Plan{}, nil, err
	}
	defer tx.End(&err)
	if plan, err = diff(c, desired, opts.Prune); err != nil { // again, under the lock
		return index.Plan{}, nil, err
	}
	for _, ch := range plan.Changes {
		if err := change(c, v, ch, opts.Prune); err != nil {
			return index.Plan{}, nil, fmt.Errorf("%s: %w", ch.Index.Name, err)
		}
	}
	return plan, nil, nil
}

// check prepares every change without making it, and returns why any would be refused.
func check(c *sqlite3.Conn, v *vetter, plan index.Plan, prune index.Prune) []error {
	var refused []error
	for _, ch := range plan.Changes {
		if err := uniqueGuard(ch, prune); err != nil {
			refused = append(refused, err)
			continue
		}
		if ch.Action == index.Drop {
			continue
		}
		sql := ch.Index.SQL
		if ch.Action == index.Replace { // the old index still holds the name
			name, err := sqlite.FreeName(c, "narcissus_check_"+ch.Index.Name)
			if err == nil {
				sql, err = ch.Index.Renamed(name)
			}
			if err != nil {
				refused = append(refused, err)
				continue
			}
		}
		if err := sqlite.PrepareIndexChange(c, sql, func(r sqlite.Reads) error { return v.vet(ch.Index, r) }); err != nil {
			refused = append(refused, fmt.Errorf("%s: %w", ch.Index.Name, err))
		}
	}
	return refused
}

// uniqueGuard asks for -prune-unique before a UNIQUE index is replaced, since
// any change to one can loosen the rule it enforces.
func uniqueGuard(ch index.Change, prune index.Prune) error {
	if ch.Action == index.Replace && ch.Old.Unique && prune != index.PruneAll {
		return fmt.Errorf("%s: replacing a UNIQUE index can loosen the rule it enforces; run with -prune -prune-unique if that's intended", ch.Index.Name)
	}
	return nil
}

// change makes one change, vetting a new index before and after it's built.
func change(c *sqlite3.Conn, v *vetter, ch index.Change, prune index.Prune) error {
	if err := uniqueGuard(ch, prune); err != nil {
		return err
	}
	if ch.Action != index.Create {
		if err := sqlite.ExecIndexChange(c, `DROP INDEX `+sqltext.Quote(ch.Index.Name), nil); err != nil {
			return err
		}
	}
	if ch.Action == index.Drop {
		return nil
	}
	if err := sqlite.ExecIndexChange(c, ch.Index.SQL, func(r sqlite.Reads) error { return v.vet(ch.Index, r) }); err != nil {
		return err
	}
	return v.built(ch.Index)
}

func diff(c *sqlite3.Conn, desired []index.Index, prune index.Prune) (index.Plan, error) {
	actual, err := sqlite.Indexes(c)
	if err != nil {
		return index.Plan{}, err
	}
	return index.Diff(desired, actual, prune), nil
}
