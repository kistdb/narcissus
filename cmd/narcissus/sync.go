package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/kistdb/narcissus/internal/fleet"
	"github.com/kistdb/narcissus/internal/index"
)

// runSync runs cmd, plan or apply, over the databases in args.
func runSync(ctx context.Context, cmd string, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet(cmd, stderr)
	indexes := fs.String("indexes", "indexes.sql", "the desired indexes")
	prune := fs.Bool("prune", false, "drop indexes that the indexes file doesn't declare")
	jobs := fs.Int("jobs", 4, "databases to handle at once")
	busy := fs.Duration("busy-timeout", 5*time.Second, "how long to wait for a locked database")
	var exitPending *bool
	if cmd == "plan" {
		exitPending = fs.Bool("exit-code", false, "exit with status 3 if any database needs changes")
	}
	if err := parse(fs, args, -1); err != nil {
		return err
	}
	if *jobs < 1 {
		return usageError{err: fmt.Errorf("%s: -jobs must be at least 1", cmd)}
	}

	src, err := os.ReadFile(*indexes)
	if err != nil {
		return err
	}
	desired, err := index.Parse(*indexes, string(src))
	if err != nil {
		return err
	}
	paths, err := fleet.Expand(fs.Args())
	if err != nil {
		return err
	}
	apply := cmd == "apply"
	results := fleet.Run(ctx, paths, desired, fleet.Options{Prune: *prune, Apply: apply, Jobs: *jobs, BusyTimeout: *busy})
	if printResults(stdout, results, *indexes, apply) {
		return errFailed
	}
	if exitPending != nil && *exitPending && slices.ContainsFunc(results, func(r fleet.Result) bool { return !r.Plan.Empty() }) {
		return errPending
	}
	return nil
}

// printResults writes each database's changes and a summary, reporting whether any failed.
func printResults(w io.Writer, results []fleet.Result, indexesFile string, applied bool) (failed bool) {
	var changed, inSync, failures int
	for _, r := range results {
		switch {
		case r.Err != nil:
			failures++
			fmt.Fprintf(w, "%s: error: %v\n", r.Path, r.Err)
			continue
		case r.Plan.Empty():
			inSync++
		default:
			changed++
		}
		if r.Plan.Empty() && len(r.Plan.Unmanaged) == 0 {
			continue
		}
		fmt.Fprintln(w, r.Path)
		for _, c := range r.Plan.Changes {
			switch c.Action {
			case index.Create:
				fmt.Fprintf(w, "  + %s\n", c.Index)
			case index.Replace:
				fmt.Fprintf(w, "  ~ %s\n", c.Index)
			case index.Drop:
				fmt.Fprintf(w, "  - DROP INDEX %s\n", c.Index.Name)
			}
		}
		for _, ix := range r.Plan.Unmanaged {
			fmt.Fprintf(w, "  ! keeping %s, which %s doesn't declare (-prune drops it)\n", ix.Name, indexesFile)
		}
	}
	verb := "to change"
	if applied {
		verb = "changed"
	}
	fmt.Fprintf(w, "%d %s: %d %s, %d in sync, %d failed.\n",
		len(results), plural(len(results), "database", "databases"), changed, verb, inSync, failures)
	return failures > 0
}
