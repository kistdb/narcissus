package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kistdb/narcissus/internal/fleet"
	"github.com/kistdb/narcissus/internal/index"
)

// runSync runs cmd, plan or apply, over the databases in args.
func runSync(ctx context.Context, cmd string, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet(cmd, stderr)
	indexes := fs.String("indexes", "indexes.sql", "the desired indexes")
	prune := fs.Bool("prune", false, "drop non-UNIQUE indexes that the indexes file doesn't declare")
	pruneUnique := fs.Bool("prune-unique", false, "with -prune, drop undeclared UNIQUE indexes too")
	allowApp := fs.Bool("allow-collations-and-functions", false,
		"create indexes that use collations other than BINARY, functions, or LIKE, GLOB, REGEXP and MATCH")
	jobs := fs.Int("jobs", 4, "databases to handle at once")
	busy := fs.Duration("busy-timeout", 5*time.Second, "how long to wait for a locked database")
	var exitPending *bool
	if cmd == "plan" {
		exitPending = fs.Bool("exit-code", false, "exit with status 3 if any database needs changes")
	}
	if err := parse(fs, args, -1); err != nil {
		return err
	}
	switch {
	case *jobs < 1:
		return usageError{err: fmt.Errorf("%s: -jobs must be at least 1", cmd)}
	case *pruneUnique && !*prune:
		return usageError{err: fmt.Errorf("%s: -prune-unique needs -prune", cmd)}
	}

	src, err := os.ReadFile(*indexes)
	if err != nil {
		return err
	}
	desired, err := index.Parse(*indexes, string(src))
	if err != nil {
		return err
	}
	if *prune && len(desired) == 0 {
		return fmt.Errorf("%s declares no indexes, so -prune would drop them all; refusing", *indexes)
	}
	paths, skipped, err := fleet.Expand(fs.Args())
	if err != nil {
		return err
	}
	for _, p := range skipped {
		fmt.Fprintf(stderr, "narcissus: skipped %s: not a regular file\n", clean(p))
	}

	opts := fleet.Options{Apply: cmd == "apply", AllowAppDependent: *allowApp, Jobs: *jobs, BusyTimeout: *busy}
	switch {
	case *pruneUnique:
		opts.Prune = index.PruneAll
	case *prune:
		opts.Prune = index.PruneNonUnique
	}
	var s summary
	fleet.Run(ctx, paths, desired, opts, func(r fleet.Result) { s.print(stdout, r, *indexes, opts) })
	s.printTotals(stdout, len(paths), opts.Apply)
	switch {
	case s.failed > 0:
		return errFailed
	case exitPending != nil && *exitPending && s.changed > 0:
		return errPending
	}
	return nil
}

// summary counts results as they're printed.
type summary struct{ changed, inSync, failed int }

// print writes one database's changes, as they come.
func (s *summary) print(w io.Writer, r fleet.Result, indexesFile string, opts fleet.Options) {
	switch {
	case r.Err != nil:
		s.failed++
		fmt.Fprintf(w, "%s: error: %s\n", clean(r.Path), clean(r.Err.Error()))
		return
	case r.Plan.Empty():
		s.inSync++
	default:
		s.changed++
	}
	if r.Plan.Empty() && len(r.Plan.Unmanaged) == 0 {
		return
	}
	fmt.Fprintln(w, clean(r.Path))
	for _, c := range r.Plan.Changes {
		switch c.Action {
		case index.Create:
			fmt.Fprintf(w, "  + %s\n", clean(c.Index.SQL))
		case index.Replace:
			fmt.Fprintf(w, "  ~ %s\n", clean(c.Index.SQL))
		case index.Drop:
			fmt.Fprintf(w, "  - DROP INDEX %s\n", clean(c.Index.Name))
		}
		if deps := c.Index.AppDependent(); c.Action != index.Drop && len(deps) > 0 && !opts.AllowAppDependent {
			fmt.Fprintf(w, "    ! uses %s: apply needs -allow-collations-and-functions\n", clean(strings.Join(deps, ", ")))
		}
		if c.Action == index.Replace && c.Old.Unique && !c.Index.Unique && opts.Prune != index.PruneAll {
			fmt.Fprintf(w, "    ! drops a UNIQUE constraint: apply needs -prune -prune-unique\n")
		}
	}
	for _, ix := range r.Plan.Unmanaged {
		how := "-prune drops it"
		if ix.Unique {
			how = "it's UNIQUE: -prune -prune-unique drops it"
		}
		fmt.Fprintf(w, "  ! keeping %s, which %s doesn't declare (%s)\n", clean(ix.Name), clean(indexesFile), how)
	}
}

func (s *summary) printTotals(w io.Writer, n int, applied bool) {
	verb := "to change"
	if applied {
		verb = "changed"
	}
	fmt.Fprintf(w, "%d %s: %d %s, %d in sync, %d failed.\n",
		n, plural(n, "database", "databases"), s.changed, verb, s.inSync, s.failed)
}

// clean quotes text that holds control characters, so output can't be forged.
func clean(s string) string {
	if strings.ContainsFunc(s, unicode.IsControl) {
		return strconv.Quote(s)
	}
	return s
}
