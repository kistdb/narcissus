package scratch

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3"

	"github.com/kistdb/narcissus/internal/sqlite"
)

const indexesSQL = `CREATE INDEX orders_customer ON orders (customer_id);
CREATE INDEX users_email ON users (email);
`

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// indexNames returns the scratch database's index names.
func indexNames(t *testing.T, d *DB) []string {
	t.Helper()
	ixs, err := sqlite.Indexes(d.Conn)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ix := range ixs {
		names = append(names, ix.Name)
	}
	return names
}

func TestBuildFromSQL(t *testing.T) {
	dir := t.TempDir()
	schema := write(t, dir, "schema.sql", `
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);
CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INT);
CREATE INDEX users_email ON users (email);
CREATE INDEX legacy ON orders (id, customer_id);
`)
	d, err := Build(t.Context(), schema, write(t, dir, "indexes.sql", indexesSQL))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got, want := indexNames(t, d), []string{"orders_customer", "users_email"}; !reflect.DeepEqual(got, want) {
		t.Errorf("indexes = %q, want %q", got, want)
	}
	if want := []string{"legacy"}; !reflect.DeepEqual(d.Ignored, want) {
		t.Errorf("Ignored = %q, want %q", d.Ignored, want)
	}
	if len(d.Indexes) != 2 {
		t.Errorf("Indexes = %+v, want 2", d.Indexes)
	}
}

func TestBuildFromDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.db")
	c, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Exec(`
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);
CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INT);
CREATE VIRTUAL TABLE docs USING fts5(body);
CREATE INDEX orders_customer ON orders (customer_id);
CREATE INDEX legacy ON orders (id, customer_id);
WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 500)
INSERT INTO orders (customer_id) SELECT i % 50 FROM n;
ANALYZE;
`)
	c.Close()
	if err != nil {
		t.Fatal(err)
	}

	d, err := Build(t.Context(), path, write(t, dir, "indexes.sql", indexesSQL))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var stat string
	err = sqlite.Query(d.Conn, `SELECT stat FROM sqlite_stat1 WHERE idx = 'orders_customer'`, func(st *sqlite3.Stmt) error {
		stat = st.ColumnText(0)
		return nil
	})
	if err != nil || stat != "500 10" {
		t.Errorf("copied stat = %q (err %v), want %q", stat, err, "500 10")
	}
	if want := []string{"legacy"}; !reflect.DeepEqual(d.Ignored, want) {
		t.Errorf("Ignored = %q, want %q", d.Ignored, want)
	}
	var rows int
	if err := sqlite.Query(d.Conn, `SELECT count(*) FROM orders`, func(st *sqlite3.Stmt) error { rows = st.ColumnInt(0); return nil }); err != nil || rows != 0 {
		t.Errorf("scratch has %d orders (err %v), want none copied", rows, err)
	}
}

func TestBuildFromMigrations(t *testing.T) {
	dir := t.TempDir()
	mig := filepath.Join(dir, "migrations")
	if err := os.Mkdir(mig, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, mig, "10_orders.up.sql", `CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INT);`)
	write(t, mig, "10_orders.down.sql", `DROP TABLE orders;`)
	write(t, mig, "2_users.sql", "-- migrate:up\nCREATE TABLE users (id INTEGER PRIMARY KEY);\n-- migrate:down\nDROP TABLE users;\n")
	write(t, mig, "20_email.sql", "-- +goose Up\n-- +goose StatementBegin\nALTER TABLE users ADD COLUMN email TEXT;\n-- +goose StatementEnd\n-- +goose Down\nSELECT 1;\n")
	write(t, mig, "README.md", "not a migration")

	d, err := Build(t.Context(), mig, write(t, dir, "indexes.sql", indexesSQL))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got, want := indexNames(t, d), []string{"orders_customer", "users_email"}; !reflect.DeepEqual(got, want) {
		t.Errorf("indexes = %q, want %q", got, want)
	}
}

func TestBuildErrors(t *testing.T) {
	dir := t.TempDir()
	schema := write(t, dir, "schema.sql", "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);\n")
	empty := filepath.Join(dir, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, schema, indexes, wantErr string
	}{
		{"missing indexes file", schema, filepath.Join(dir, "nope.sql"), "no such file"},
		{"not an index", schema, write(t, dir, "bad.sql", "DROP TABLE users;"), "bad.sql:1: want only CREATE INDEX"},
		{"unknown table", schema, write(t, dir, "orders.sql", "\nCREATE INDEX o ON orders (x);"), "orders.sql:2: "},
		{"bad schema", write(t, dir, "broken.sql", "CREATE TABLE ("), write(t, dir, "ok.sql", ""), "broken.sql:1: "},
		{"no migrations", empty, write(t, dir, "ok2.sql", ""), "no .sql migrations"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := Build(t.Context(), tt.schema, tt.indexes)
			if err == nil {
				d.Close()
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Build() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestMigrationOrder(t *testing.T) {
	names := []string{"V10__b.sql", "V2__a.sql", "init.sql", "20240101_x.sql", "003_c.sql", "3_b.sql"}
	slices.SortFunc(names, compareMigrations)
	want := []string{"init.sql", "V2__a.sql", "003_c.sql", "3_b.sql", "V10__b.sql", "20240101_x.sql"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("order = %q, want %q", names, want)
	}
}

func TestUpSection(t *testing.T) {
	tests := []struct {
		text, want string
		line       int
	}{
		{"CREATE TABLE a (x);\n", "CREATE TABLE a (x);\n", 1},
		{"-- comment\n-- migrate:up transaction:false\nA;\n-- migrate:down\nB;\n", "A;\n", 3},
		{"-- +goose Up\nA;\nB;\n", "A;\nB;\n", 2},
	}
	for _, tt := range tests {
		got, line := upSection(tt.text)
		if got != tt.want || line != tt.line {
			t.Errorf("upSection(%q) = %q, %d; want %q, %d", tt.text, got, line, tt.want, tt.line)
		}
	}
}

func TestBuildCantTouchFiles(t *testing.T) {
	dir := t.TempDir()
	prod := filepath.Join(dir, "prod.db")
	c, err := sqlite3.Open(prod)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(`CREATE TABLE users (id); INSERT INTO users VALUES (1), (2)`); err != nil {
		t.Fatal(err)
	}
	c.Close()
	before, _ := os.ReadFile(prod)
	newFile := filepath.Join(dir, "new.db")
	for name, schema := range map[string]string{
		"attach":      "CREATE TABLE t (a);\nATTACH 'file:" + prod + "?mode=rw' AS p;\nDELETE FROM p.users;",
		"vacuum into": "CREATE TABLE t (a);\nVACUUM INTO '" + newFile + "';",
	} {
		d, err := Build(t.Context(), write(t, dir, "schema.sql", schema), write(t, dir, "indexes.sql", ""))
		if err == nil {
			d.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "can't be used here") {
			t.Errorf("%s: Build() = %v, want a refusal", name, err)
		}
	}
	if after, _ := os.ReadFile(prod); !bytes.Equal(before, after) {
		t.Error("prod.db changed")
	}
	if _, err := os.Stat(newFile); err == nil {
		t.Error("VACUUM INTO created a file")
	}
}

func TestMigrationLayouts(t *testing.T) {
	dir := t.TempDir()
	mig := filepath.Join(dir, "migrations")
	for _, d := range []string{"", "20240101_init", "20240202_orders"} {
		if err := os.MkdirAll(filepath.Join(mig, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(mig, "20240101_init"), "migration.sql", "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);")
	write(t, filepath.Join(mig, "20240202_orders"), "migration.sql", "CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INT);")
	write(t, mig, "20240303_note.sql", "-- +migrate Up\nALTER TABLE orders ADD COLUMN note TEXT;\n-- +migrate Down\nDROP TABLE orders;\n")
	d, err := Build(t.Context(), mig, write(t, dir, "indexes.sql", indexesSQL))
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
}

func TestDatabaseIndexesItCantBuild(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.db")
	c, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CreateCollation("app_order", func(a, b []byte) int { return bytes.Compare(a, b) }); err != nil {
		t.Fatal(err)
	}
	err = c.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT); CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INT);
		CREATE INDEX users_app ON users (email COLLATE app_order);`)
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	d, err := Build(t.Context(), path, write(t, dir, "indexes.sql", indexesSQL))
	if err != nil {
		t.Fatalf("Build() = %v, want the unbuildable index left out", err)
	}
	defer d.Close()
	if !slices.Contains(d.Ignored, "users_app") {
		t.Errorf("Ignored = %q, want users_app", d.Ignored)
	}
}
