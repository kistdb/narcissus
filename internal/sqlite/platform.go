//go:build linux || darwin || windows

package sqlite

// writeSupported is false where the SQLite library's file locking may not
// match other SQLite programs' (its own docs warn of corruption there).
const writeSupported = true
