package sqlite

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
)

// newDB creates a database at path by running sql with the stock settings of this library.
func newDB(t *testing.T, path, sql string) {
	t.Helper()
	c, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Exec(sql); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRefuses(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "app.db")
	newDB(t, db, `CREATE TABLE t (a)`)
	empty := filepath.Join(dir, "empty.db")
	text := filepath.Join(dir, "notes.db")
	for path, content := range map[string]string{empty: "", text: "hello, not a database"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	deep := dir
	for len(deep) < maxPath { // over maxPath, but under macOS's 1024-byte limit
		deep = filepath.Join(deep, strings.Repeat("x", min(200, maxPath+20-len(deep))))
	}
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	deep = filepath.Join(deep, "app.db")
	newDB(t, deep, `CREATE TABLE t (a)`)
	linked := filepath.Join(dir, "linked.db")
	if err := os.Link(db, linked); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, path string
		write      bool
		wantErr    string
	}{
		{"empty file", empty, false, "not a SQLite database"},
		{"text file", text, false, "not a SQLite database"},
		{"folder", dir, false, "not a SQLite database"},
		{"hard link, write", linked, true, "2 names"},
		{"hard link, read", linked, false, "2 names"},
		{"long path", deep, false, "longer than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Open(t.Context(), tt.path, Options{Write: tt.write})
			if err == nil {
				c.Close()
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Open(%s) = %v, want %q", tt.name, err, tt.wantErr)
			}
		})
	}
	if c, err := Open(t.Context(), linked, Options{}); err == nil {
		c.Close()
		t.Error("read-only Open of a hard-linked database succeeded")
	}
	if b, _ := os.ReadFile(empty); len(b) != 0 {
		t.Error("Open wrote to an empty file")
	}
}

func TestOpenFileNamedLikeURI(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	newDB(t, "t.db", `CREATE TABLE plain (a)`)
	newDB(t, "literal.db", `CREATE TABLE literal (a)`)
	if err := os.Rename("literal.db", "file:t.db"); err != nil {
		t.Fatal(err)
	}
	c, err := Open(t.Context(), "file:t.db", Options{Write: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var name string
	if err := Query(c, `SELECT name FROM sqlite_schema`, func(st *sqlite3.Stmt) error { name = st.ColumnText(0); return nil }); err != nil || name != "literal" {
		t.Errorf("opened the table %q (err %v), want the file literally named file:t.db", name, err)
	}
}

func TestOpenWaitsForLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	newDB(t, path, `CREATE TABLE t (a)`)
	holder, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := holder.Exec(`BEGIN EXCLUSIVE; INSERT INTO t VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	committed := make(chan error)
	go func() {
		time.Sleep(300 * time.Millisecond)
		committed <- holder.Exec(`COMMIT`)
	}()
	c, err := Open(t.Context(), path, Options{BusyTimeout: 5 * time.Second})
	if cerr := <-committed; cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatalf("Open() while locked = %v, want it to wait", err)
	}
	c.Close()
}

func TestImmutableOnlyWhenComplete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "app.db")
	newDB(t, path, `PRAGMA journal_mode = WAL; CREATE TABLE t (a)`)
	// A writer that leaves its last change in the WAL.
	w, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Exec(`PRAGMA wal_autocheckpoint = 0; CREATE INDEX t_a ON t (a)`); err != nil {
		t.Fatal(err)
	}
	snapshot := t.TempDir()
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(snapshot, "app.db"+suffix), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	if err := os.Chmod(snapshot, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(snapshot, 0o700) })

	c, err := Open(t.Context(), filepath.Join(snapshot, "app.db"), Options{})
	if err == nil {
		c.Close()
		t.Error("Open() read a snapshot as immutable although its WAL holds changes")
	}
}

func TestExecIndexChange(t *testing.T) {
	c := memory(t)
	if err := c.Exec(`CREATE TABLE t (a, b); CREATE TABLE precious (x); INSERT INTO precious VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`CREATE INDEX i ON t (lower(a)) WHERE b > abs(1)`,
		`DROP INDEX i`,
		`CREATE UNIQUE INDEX "odd ""name""" ON t (a)`,
		`DROP INDEX "odd ""name"""`,
	} {
		if err := ExecIndexChange(c, sql, nil); err != nil {
			t.Errorf("ExecIndexChange(%q) = %v", sql, err)
		}
	}
	for _, sql := range []string{
		`DROP TABLE precious`,
		`CREATE INDEX j ON t (a); DROP TABLE precious`,
		`DELETE FROM precious`,
		`CREATE TEMP TABLE x (a)`,
		`PRAGMA writable_schema = ON`,
	} {
		if err := ExecIndexChange(c, sql, nil); err == nil {
			t.Errorf("ExecIndexChange(%q) succeeded", sql)
		}
	}
	var n int
	if err := Query(c, `SELECT count(*) FROM precious`, func(st *sqlite3.Stmt) error { n = st.ColumnInt(0); return nil }); err != nil || n != 1 {
		t.Errorf("precious has %d rows (err %v), want 1", n, err)
	}
}

func TestNoAttach(t *testing.T) {
	c := memory(t)
	target := filepath.Join(t.TempDir(), "new.db")
	for _, sql := range []string{`ATTACH ':memory:' AS x`, `VACUUM INTO '` + target + `'`} {
		if err := c.Exec(sql); err == nil {
			t.Errorf("%s succeeded", sql)
		}
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("VACUUM INTO created a file")
	}
}

func TestOpenHugeBusyTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	newDB(t, path, `CREATE TABLE t (a)`)
	c, err := Open(t.Context(), path, Options{BusyTimeout: 10000 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}
