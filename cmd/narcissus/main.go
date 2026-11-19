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
)

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), stopSignals...)
	ctx, cancel := context.WithCancel(ctx)
	ignoreBrokenPipe()
	err := run(ctx, os.Args[1:], &cancelOnError{w: os.Stdout, cancel: cancel}, os.Stderr)
	cancel()
	stop()
	os.Exit(exitCode(err, os.Stderr))
}

// cancelOnError stops the run once output can't be written, as when piped to head,
// so work in progress is rolled back rather than cut off.
type cancelOnError struct {
	w      io.Writer
	cancel context.CancelFunc
}

func (c *cancelOnError) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if err != nil {
		c.cancel()
	}
	return n, err
}

// exitCode returns the exit status for run's error, printing it if needed.
func exitCode(err error, stderr io.Writer) int {
	usage, isUsage := errors.AsType[usageError](err)
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errFailed):
		return 1 // already reported
	case errors.Is(err, errPending):
		return 3
	case isUsage:
		if !usage.printed {
			fmt.Fprintln(stderr, "narcissus:", err)
		}
		return 2
	default:
		fmt.Fprintln(stderr, "narcissus:", err)
		return 1
	}
}
