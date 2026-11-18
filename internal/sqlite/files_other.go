//go:build !unix

package sqlite

import (
	"io/fs"
	"os"
)

func openNoBlock(path string) (*os.File, error) { return os.Open(path) }

func matchShm(string, fs.FileInfo) {}
