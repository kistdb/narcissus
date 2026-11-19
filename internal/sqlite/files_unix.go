//go:build unix

package sqlite

import (
	"io/fs"
	"os"
	"syscall"
)

func openNoBlock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}

// matchShm gives a -shm file the database's permissions and, run as root, its
// owner, as stock SQLite does. It works through one handle, so a swap can't redirect it.
func matchShm(abs string, db fs.FileInfo) {
	f, err := openNoBlock(abs + "-shm")
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	have, ok1 := fi.Sys().(*syscall.Stat_t)
	want, ok2 := db.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 || have.Nlink != 1 {
		return
	}
	if fi.Mode().Perm() != db.Mode().Perm() {
		_ = f.Chmod(db.Mode().Perm())
	}
	if os.Geteuid() == 0 && (have.Uid != want.Uid || have.Gid != want.Gid) {
		_ = f.Chown(int(want.Uid), int(want.Gid))
	}
}
