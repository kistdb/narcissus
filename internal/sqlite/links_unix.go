//go:build unix

package sqlite

import (
	"io/fs"
	"syscall"
)

// links returns how many names the file has.
func links(_ string, fi fs.FileInfo) (uint64, error) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink), nil
	}
	return 1, nil
}
