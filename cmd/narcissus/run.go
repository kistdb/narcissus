package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
)

const usageText = `Usage:
  narcissus check [flags]          check that queries.sql uses indexes
  narcissus plan [flags] DB...     show the index changes each database needs
  narcissus apply [flags] DB...    make them; DB may be a glob, like 'tenants/*.db'
  narcissus init [flags] DB        write schema.sql and indexes.sql from a database
  narcissus version                print the version

Run a command with -h for its flags.
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
	case "init":
		return runInit(ctx, args[1:], stdout, stderr)
	case "plan", "apply":
		return runSync(ctx, cmd, args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, buildVersion())
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
