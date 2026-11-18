//go:build !unix && !windows

package sqlite

import "io/fs"

func links(string, fs.FileInfo) (uint64, error) { return 1, nil }
