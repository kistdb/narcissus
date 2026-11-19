//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// stopSignals end a run cleanly: the database being changed is rolled back.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// ignoreBrokenPipe turns a closed stdout into a write error, not a sudden exit.
func ignoreBrokenPipe() { signal.Ignore(syscall.SIGPIPE) }
