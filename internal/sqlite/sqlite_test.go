package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3"
)

func memory(t *testing.T) *sqlite3.Conn {
	t.Helper()
	c, err := OpenMemory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestScriptRun(t *testing.T) {
	c := memory(t)
	err := Script{File: "schema.sql", Line: 1, Text: `
CREATE TABLE t (a, b);
CREATE TRIGGER t_ins AFTER INSERT ON t BEGIN
  UPDATE t SET b = CASE WHEN new.a > 0 THEN 1 END;
  UPDATE t SET b = b WHERE a = new.a;
END;
INSERT INTO t (a) VALUES (1);
`}.Run(c)
	if err != nil {
		t.Fatal(err)
	}
	var b int
	if err := Query(c, `SELECT b FROM t`, func(st *sqlite3.Stmt) error { b = st.ColumnInt(0); return nil }); err != nil || b != 1 {
		t.Errorf("trigger didn't run: b = %d, err %v", b, err)
	}

	err = Script{File: "002_up.sql", Line: 10, Text: "CREATE TABLE u (a);\n\n-- bad\nCREATE INDEX u_x ON u (missing);"}.Run(c)
	if err == nil || !strings.HasPrefix(err.Error(), "002_up.sql:13: ") || !strings.Contains(err.Error(), "missing") {
		t.Errorf("Run() error = %v, want it at 002_up.sql:13", err)
	}
}

func TestSchemaAndIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	c, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Exec(`
CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT UNIQUE);
CREATE INDEX users_email_lower ON users (lower(email));
CREATE VIRTUAL TABLE docs USING fts5(body);
CREATE VIEW v AS SELECT * FROM users;
CREATE TRIGGER tr AFTER DELETE ON users BEGIN DELETE FROM docs; END;
CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INT);
CREATE TABLE docs_meta (x);
CREATE TABLE docs_segments (x);
CREATE INDEX 'quoted name' ON 'orders' ('user_id');
INSERT INTO users (email) VALUES ('a');
`)
	c.Close()
	if err != nil {
		t.Fatal(err)
	}

	c, err = Open(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	objs, err := Schema(c)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range objs {
		got = append(got, o.Type+" "+o.Name)
	}
	want := []string{"table users", "table docs", "table orders", "table docs_meta", "view v", "trigger tr"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Schema() = %q, want %q", got, want)
	}

	ixs, err := Indexes(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(ixs) != 2 || ixs[0].Name != "users_email_lower" || ixs[0].Table != "users" || ixs[1].Name != "quoted name" {
		t.Errorf("Indexes() = %+v, want users_email_lower and quoted name", ixs)
	}

	if err := c.Exec(`CREATE TABLE x (a)`); err == nil {
		t.Error("read-only connection allowed a write")
	}
	if _, err := Open(t.Context(), filepath.Join(t.TempDir(), "missing.db"), Options{Write: true}); err == nil {
		t.Error("Open created a missing database")
	}
}

func TestStockDefaults(t *testing.T) {
	c := memory(t)
	err := Script{File: "schema.sql", Line: 1, Text: `
CREATE TABLE parent (id INTEGER PRIMARY KEY);
CREATE TABLE child (parent_id INT REFERENCES parent (id), status TEXT DEFAULT "open" CHECK (status IN ("open", "done")));
INSERT INTO child (parent_id) VALUES (42);
`}.Run(c)
	if err != nil {
		t.Errorf("double-quoted strings or foreign keys not as in stock SQLite: %v", err)
	}
}

func TestScriptRunInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cancel()
	if err := (Script{File: "s.sql", Line: 1, Text: "CREATE TABLE t (a);"}).Run(c); err == nil {
		t.Error("Run() succeeded after cancel")
	}
}

func TestOpenWALInReadOnlyFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod can't make a Windows folder read-only")
	}
	dir := filepath.Join(t.TempDir(), "snap shots?")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "app.db")
	c, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Exec(`PRAGMA journal_mode = WAL; CREATE TABLE t (a); CREATE INDEX t_a ON t (a);`)
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	c, err = Open(t.Context(), path, Options{})
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer c.Close()
	if ixs, err := Indexes(c); err != nil || len(ixs) != 1 {
		t.Errorf("Indexes() = %+v, %v; want t_a", ixs, err)
	}
	if _, err := Open(t.Context(), path, Options{Write: true}); err == nil {
		t.Error("Open(write) succeeded in a read-only folder")
	}
}
