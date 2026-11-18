// Package fleet plans and applies index changes across many databases.
package fleet

import (
	"context"
	"errors"
	"fmt"
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
	Prune       bool // drop indexes the desired set doesn't declare
	Apply       bool // make the changes, not just plan them
	Jobs        int  // databases handled at once
	BusyTimeout time.Duration
}

// Result is the outcome for one database.
type Result struct {
	Path string
	Plan index.Plan
	Err  error
}

// Expand returns the database files args name, expanding glob patterns and
// skipping SQLite's -wal, -shm and -journal files among their matches.
func Expand(args []string) ([]string, error) {
	var paths []string
	for _, arg := range args {
		if fi, err := os.Stat(arg); err == nil || !strings.ContainsAny(arg, "*?[") {
			if err != nil {
				return nil, err
			}
			if fi.IsDir() {
				return nil, fmt.Errorf("%s is a directory", arg)
			}
			paths = append(paths, arg)
			continue
		}
		matches, err := filepath.Glob(arg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", arg, err)
		}
		matches = slices.DeleteFunc(matches, func(m string) bool {
			fi, err := os.Stat(m)
			return err != nil || fi.IsDir() ||
				strings.HasSuffix(m, "-wal") || strings.HasSuffix(m, "-shm") || strings.HasSuffix(m, "-journal")
		})
		if len(matches) == 0 {
			return nil, fmt.Errorf("no databases match %s", arg)
		}
		paths = append(paths, matches...)
	}
	seen := make(map[string]bool, len(paths))
	return slices.DeleteFunc(paths, func(p string) bool {
		dup := seen[p]
		seen[p] = true
		return dup
	}), nil
}

// Run plans, and with opts.Apply makes, each database's changes, opts.Jobs
// at a time. Results follow the order of paths.
func Run(ctx context.Context, paths []string, desired []index.Index, opts Options) []Result {
	results := make([]Result, len(paths))
	sem := make(chan struct{}, max(opts.Jobs, 1))
	var wg sync.WaitGroup
	for i, path := range paths {
		results[i].Path = path
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			results[i].Err = ctx.Err()
			continue
		}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i].Plan, results[i].Err = one(ctx, path, desired, opts)
		})
	}
	wg.Wait()
	return results
}

// one plans, and maybe applies, the changes for the database at path. It
// takes the write lock only when there is something to change.
func one(ctx context.Context, path string, desired []index.Index, opts Options) (_ index.Plan, err error) {
	c, err := sqlite.Open(ctx, path, opts.Apply)
	if err != nil {
		return index.Plan{}, err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	if err := c.BusyTimeout(opts.BusyTimeout); err != nil {
		return index.Plan{}, err
	}
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
		if ch.Action != index.Create {
			if err := c.Exec(`DROP INDEX ` + sqltext.Quote(ch.Index.Name)); err != nil {
				return index.Plan{}, err
			}
		}
		if ch.Action != index.Drop {
			if err := c.Exec(ch.Index.SQL); err != nil {
				return index.Plan{}, fmt.Errorf("%s: %w", ch.Index.Name, err)
			}
		}
	}
	return plan, nil
}

func diff(c *sqlite3.Conn, desired []index.Index, prune bool) (index.Plan, error) {
	actual, err := sqlite.Indexes(c)
	if err != nil {
		return index.Plan{}, err
	}
	return index.Diff(desired, actual, prune), nil
}
