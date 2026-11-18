// Command narcissus checks that SQLite queries use indexes, and keeps
// databases' indexes in line with an indexes.sql file.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(exitCode(err, os.Stderr))
}

// exitCode returns the exit status for run's error, printing it if needed.
func exitCode(err error, stderr io.Writer) int {
	var usage usageError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errFailed):
		return 1 // already reported
	case errors.Is(err, errPending):
		return 3
	case errors.As(err, &usage):
		if !usage.printed {
			fmt.Fprintln(stderr, "narcissus:", err)
		}
		return 2
	default:
		fmt.Fprintln(stderr, "narcissus:", err)
		return 1
	}
}
