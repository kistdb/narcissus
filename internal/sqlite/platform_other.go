//go:build !(linux || darwin || windows)

package sqlite

const writeSupported = false
