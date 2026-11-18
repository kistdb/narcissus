//go:build unix

package sqlite

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
)

func TestOpenRefusesFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "pipe.db")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), fifo, Options{}); err == nil || !strings.Contains(err.Error(), "not a SQLite database") {
		t.Errorf("Open(fifo) = %v, want not a SQLite database", err)
	}
}

func TestImmutableThroughSymlink(t *testing.T) {
	ro := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(ro, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ro, "snap.db")
	newDB(t, path, `PRAGMA journal_mode = WAL; CREATE TABLE t (a)`)
	if err := os.WriteFile(path+"-wal", []byte("frames not yet checkpointed"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.db")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	if c, err := Open(t.Context(), link, Options{}); err == nil {
		c.Close()
		t.Error("Open(symlink) read a snapshot as immutable although its WAL holds changes")
	}
}

func TestShmMatchesDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	newDB(t, path, `PRAGMA journal_mode = WAL; CREATE TABLE t (a)`)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	c, err := Open(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fi, err := os.Stat(path + "-shm")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("-shm mode %v, want the database's 0640", fi.Mode().Perm())
	}
}

func TestVerifyCatchesSwap(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
	newDB(t, a, `CREATE TABLE t (a)`)
	newDB(t, b, `CREATE TABLE t (a)`)
	c, err := Open(t.Context(), a, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	other, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(c, a, other, false); err == nil || !strings.Contains(err.Error(), "changed while it was being opened") {
		t.Errorf("verify() against another file = %v, want a refusal", err)
	}
}

func TestOpenFileGivesUpOnFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "pipe.db")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := openFile(ctx, uri(fifo, url.Values{"mode": {"ro"}}), sqlite3.OPEN_URI|sqlite3.OPEN_READONLY, time.Second)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("openFile(fifo) = %v after %v, want to give up on cancel", err, time.Since(start))
	}
	// Unblock the abandoned open so the test leaves nothing behind.
	if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
		w.Close()
	}
}
