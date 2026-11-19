package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"

	"github.com/ncruces/go-sqlite3"

	"github.com/kistdb/narcissus/internal/sqlite"
)

const usageText = `Usage:
  narcissus check [flags]          check that the queries in queries.sql use indexes
  narcissus plan  [flags] DB...    show the index changes each database needs
  narcissus apply [flags] DB...    make those changes
  narcissus init  [flags] DB       write schema.sql, indexes.sql and a starter queries.sql
  narcissus collations [flags] DB... list collations other SQLite programs don't have
  narcissus convert [flags] DB NEW show, then with -write make, a copy using built-in collations
  narcissus version                print the version

DB is a database file or a quoted glob such as 'tenants/*.db'. Flags go before it.
Run a command with -h to see its flags.

Only apply changes your databases. Please keep backups and run plan first;
Narcissus comes with no warranty.
`

// errFailed reports a failure the command has already described.
var errFailed = errors.New("failed")

// errPending reports, for plan -exit-code, that databases need changes.
var errPending = errors.New("changes pending")

// usageError is a command-line mistake; printed is set once it's been reported.
type usageError struct {
	err     error
	printed bool
}

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// run runs the command line args, without the program name.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return usageError{err: errors.New("no command given")}
	}
	switch cmd := args[0]; cmd {
	case "check":
		return runCheck(ctx, args[1:], stdout, stderr)
	case "collations":
		return runCollations(ctx, args[1:], stdout, stderr)
	case "convert":
		return runConvert(ctx, args[1:], stdout, stderr)
	case "init":
		return runInit(ctx, args[1:], stdout, stderr)
	case "plan", "apply":
		return runSync(ctx, cmd, args[1:], stdout, stderr)
	case "version", "-version", "--version":
		fmt.Fprintf(stdout, "narcissus %s (SQLite %s)\n", buildVersion(), sqliteVersion())
		return nil
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usageText)
		return nil
	default:
		fmt.Fprint(stderr, usageText)
		return usageError{err: fmt.Errorf("unknown command %q", cmd)}
	}
}

// buildVersion returns the linked-in version, else the module version, else "devel".
func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "devel"
}

// sqliteVersion is the version of the SQLite built into Narcissus.
func sqliteVersion() string {
	c, err := sqlite.OpenMemory(context.Background())
	if err != nil {
		return "unknown"
	}
	defer c.Close()
	v := "unknown"
	_ = sqlite.Query(c, `SELECT sqlite_version()`, func(st *sqlite3.Stmt) error { v = st.ColumnText(0); return nil })
	return v
}
