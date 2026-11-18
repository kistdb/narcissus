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
// owner, as stock SQLite does; the library uses the umask instead.
func matchShm(abs string, db fs.FileInfo) {
	shm := abs + "-shm"
	fi, err := os.Lstat(shm)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	if fi.Mode().Perm() != db.Mode().Perm() {
		_ = os.Chmod(shm, db.Mode().Perm())
	}
	st, ok := db.Sys().(*syscall.Stat_t)
	if have, ok2 := fi.Sys().(*syscall.Stat_t); ok && ok2 && os.Geteuid() == 0 && (have.Uid != st.Uid || have.Gid != st.Gid) {
		_ = os.Lchown(shm, int(st.Uid), int(st.Gid))
	}
}
