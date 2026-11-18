package fleet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"

	"github.com/kistdb/narcissus/internal/index"
	"github.com/kistdb/narcissus/internal/sqlite"
)

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

func indexNames(t *testing.T, path string) []string {
	t.Helper()
	c, err := sqlite.Open(t.Context(), path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ixs, err := sqlite.Indexes(c)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ix := range ixs {
		names = append(names, ix.Name)
	}
	return names
}

const tables = `CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INT, status TEXT);`

func desired(t *testing.T) []index.Index {
	t.Helper()
	ixs, err := index.Parse("indexes.sql", "CREATE INDEX orders_customer ON orders (customer_id);\nCREATE INDEX orders_status ON orders (status);")
	if err != nil {
		t.Fatal(err)
	}
	return ixs
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "fresh.db")
	stale := filepath.Join(dir, "stale.db")
	broken := filepath.Join(dir, "broken.db")
	newDB(t, fresh, tables)
	newDB(t, stale, tables+`CREATE INDEX orders_status ON orders (status DESC); CREATE INDEX legacy ON orders (id, status);`)
	newDB(t, broken, `CREATE TABLE other (x);`)

	paths := []string{fresh, stale, broken}
	plans := Run(t.Context(), paths, desired(t), Options{Jobs: 2}, nil)
	if plans[0].Err != nil || plans[1].Err != nil || plans[2].Err != nil {
		t.Fatalf("plan errors: %v, %v, %v", plans[0].Err, plans[1].Err, plans[2].Err)
	}
	if n := len(plans[1].Plan.Changes); n != 2 || len(plans[1].Plan.Unmanaged) != 1 {
		t.Errorf("stale plan = %+v, want 2 changes and 1 unmanaged", plans[1].Plan)
	}
	if got := indexNames(t, fresh); got != nil {
		t.Errorf("plan changed fresh.db: %q", got)
	}

	results := Run(t.Context(), paths, desired(t), Options{Apply: true, Prune: index.PruneAll, Jobs: 2, BusyTimeout: time.Second}, nil)
	for i, want := range [][]string{{"orders_customer", "orders_status"}, {"orders_customer", "orders_status"}, nil} {
		if got := indexNames(t, paths[i]); !reflect.DeepEqual(sorted(got), want) {
			t.Errorf("%s indexes = %q, want %q", filepath.Base(paths[i]), got, want)
		}
	}
	if results[2].Err == nil || !strings.Contains(results[2].Err.Error(), "no such table") {
		t.Errorf("broken.db error = %v, want no such table", results[2].Err)
	}

	again := Run(t.Context(), paths[:2], desired(t), Options{Apply: true, Prune: index.PruneAll}, nil)
	for _, r := range again {
		if r.Err != nil || !r.Plan.Empty() {
			t.Errorf("second apply to %s: %+v", r.Path, r)
		}
	}
}

func TestRunCanceled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.db")
	newDB(t, path, tables)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, r := range Run(ctx, []string{path, path}, desired(t), Options{Apply: true, Jobs: 1}, nil) {
		if r.Err == nil {
			t.Errorf("%s: no error after cancel", r.Path)
		}
	}
	if got := indexNames(t, path); got != nil {
		t.Errorf("canceled run changed the database: %q", got)
	}
}

func TestExpand(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"b.db", "a.db", "a.db-wal", "a.db-shm", "notes.txt", "t[1].sqlite3"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")

	got, _, err := Expand([]string{b, filepath.Join(dir, "*.db*"), a})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{b, a}; !reflect.DeepEqual(got, want) {
		t.Errorf("Expand() = %q, want %q", got, want)
	}
	literal := filepath.Join(dir, "t[1].sqlite3")
	if got, _, err := Expand([]string{literal}); err != nil || !reflect.DeepEqual(got, []string{literal}) {
		t.Errorf("Expand(%q) = %q, %v; want the file itself", literal, got, err)
	}

	for _, args := range [][]string{
		{filepath.Join(dir, "missing.db")},
		{filepath.Join(dir, "*.sqlite")},
		{filepath.Join(dir, "sub.db")},
		{filepath.Join(dir, "[")},
	} {
		if _, _, err := Expand(args); err == nil {
			t.Errorf("Expand(%q) succeeded, want an error", args)
		}
	}
}

func sorted(s []string) []string {
	if s == nil {
		return nil
	}
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

func TestApplyInSyncSkipsWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	newDB(t, path, tables+`CREATE INDEX orders_customer ON orders (customer_id); CREATE INDEX orders_status ON orders (status);`)
	writer, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Exec(`BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Exec(`ROLLBACK`) }()

	r := Run(t.Context(), []string{path}, desired(t), Options{Apply: true}, nil)[0]
	if r.Err != nil || !r.Plan.Empty() {
		t.Errorf("apply to an in-sync database with a writer: %+v", r)
	}
}

func TestExpandSameFile(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "acme.db")
	newDB(t, db, tables)
	if err := os.Symlink(db, filepath.Join(dir, "alias.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(db, filepath.Join(dir, "acme-snapshot.db")); err != nil {
		t.Fatal(err)
	}
	got, _, err := Expand([]string{filepath.Join(dir, "*.db"), db})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("Expand() = %q, want the one file once", got)
	}
	r := Run(t.Context(), got, desired(t), Options{Apply: true}, nil)[0]
	if r.Err == nil || !strings.Contains(r.Err.Error(), "hard links") {
		t.Errorf("apply to a hard-linked database: %v, want a refusal", r.Err)
	}
}

func TestAppDependentIndexes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.db")
	newDB(t, path, `CREATE TABLE people (name TEXT COLLATE NOCASE, email TEXT);`)
	for _, sql := range []string{
		`CREATE INDEX people_name ON people (name);`,
		`CREATE INDEX people_email ON people (lower(email));`,
		`CREATE INDEX people_email ON people (email COLLATE NOCASE);`,
	} {
		want, err := index.Parse("indexes.sql", sql)
		if err != nil {
			t.Fatal(err)
		}
		before := indexNames(t, path)
		r := Run(t.Context(), []string{path}, want, Options{Apply: true, Prune: index.PruneAll}, nil)[0]
		if r.Err == nil || !strings.Contains(r.Err.Error(), "-allow-collations-and-functions") {
			t.Errorf("%s: %v, want a refusal", sql, r.Err)
		}
		if got := indexNames(t, path); !reflect.DeepEqual(got, before) {
			t.Errorf("%s: refused change left indexes %q, want %q", sql, got, before)
		}
		r = Run(t.Context(), []string{path}, want, Options{Apply: true, AllowAppDependent: true, Prune: index.PruneAll}, nil)[0]
		if r.Err != nil {
			t.Errorf("%s with the flag: %v", sql, r.Err)
		}
	}
}

func TestRunReportsInOrder(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for i := range 20 {
		p := filepath.Join(dir, fmt.Sprintf("t%02d.db", i))
		newDB(t, p, tables)
		paths = append(paths, p)
	}
	var got []string
	Run(t.Context(), paths, desired(t), Options{Jobs: 8}, func(r Result) { got = append(got, r.Path) })
	if !reflect.DeepEqual(got, paths) {
		t.Errorf("reported %q, want %q", got, paths)
	}
}

func TestAppDependentFromReads(t *testing.T) {
	const schema = `CREATE TABLE people (
		name TEXT COLLATE NOCASE, email TEXT, j TEXT,
		g TEXT AS (lower(email)) VIRTUAL, s TEXT AS (email || 'x') STORED);`
	refused := []string{
		`CREATE INDEX i ON people (g)`,
		`CREATE INDEX i ON people (email) WHERE name = 'x'`,
		`CREATE INDEX i ON people ((name = 'ab'))`,
		`CREATE INDEX i ON people ("lower"(email))`,
		"CREATE INDEX i ON people (`lower`(email))",
		`CREATE INDEX i ON people (replace(email, 'a', 'b'))`,
		"CREATE INDEX i ON people (lower \v(email))",
		`CREATE INDEX i ON people (j ->> '$.a')`,
		`CREATE INDEX i ON people (email) WHERE email LIKE 'a%'`,
		`CREATE INDEX i ON people (email COLLATE "nocase")`,
	}
	allowed := []string{
		`CREATE INDEX i ON people (email, s)`,
		`CREATE INDEX i ON people (email COLLATE binary)`,
		`CREATE INDEX i ON people (email) WHERE email IS NOT NULL AND s > 'a'`,
	}
	for _, sql := range append(refused, allowed...) {
		path := filepath.Join(t.TempDir(), "a.db")
		newDB(t, path, schema)
		want, err := index.Parse("indexes.sql", sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		r := Run(t.Context(), []string{path}, want, Options{Apply: true}, nil)[0]
		isRefused := r.Err != nil && strings.Contains(r.Err.Error(), "-allow-collations-and-functions")
		if wantRefused := slices.Contains(refused, sql); isRefused != wantRefused {
			t.Errorf("%s: err %v, want refused %v", sql, r.Err, wantRefused)
		}
	}
}

func TestUniqueProtected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	newDB(t, path, `CREATE TABLE t (x, y); CREATE UNIQUE INDEX u ON t (y) /* one row per y`)
	other, err := index.Parse("indexes.sql", `CREATE INDEX t_x ON t (x);`)
	if err != nil {
		t.Fatal(err)
	}
	r := Run(t.Context(), []string{path}, other, Options{Apply: true, Prune: index.PruneNonUnique}, nil)[0]
	if r.Err != nil || slices.Contains(indexNames(t, path), "u") == false {
		t.Errorf("-prune dropped a UNIQUE index SQLite stored with a trailing comment: %+v", r)
	}

	plain, err := index.Parse("indexes.sql", `CREATE INDEX u ON t (y);`)
	if err != nil {
		t.Fatal(err)
	}
	r = Run(t.Context(), []string{path}, plain, Options{Apply: true, Prune: index.PruneNonUnique}, nil)[0]
	if r.Err == nil || !strings.Contains(r.Err.Error(), "-prune-unique") {
		t.Errorf("replacing UNIQUE u with a plain index: %v, want a refusal", r.Err)
	}
	if r = Run(t.Context(), []string{path}, plain, Options{Apply: true, Prune: index.PruneAll}, nil)[0]; r.Err != nil {
		t.Errorf("with -prune-unique: %v", r.Err)
	}
}

func TestReplaceOnAnalyzedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	newDB(t, path, tables+`CREATE INDEX orders_status ON orders (status DESC); CREATE INDEX extra ON orders (id, status);
		INSERT INTO orders (customer_id, status) VALUES (1, 'a'), (2, 'b'); ANALYZE;`)
	r := Run(t.Context(), []string{path}, desired(t), Options{Apply: true, Prune: index.PruneNonUnique}, nil)[0]
	if r.Err != nil {
		t.Errorf("replace and prune on an ANALYZEd database: %v", r.Err)
	}
}

func TestGlobUnreadableFolder(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("needs folder permissions")
	}
	dir := t.TempDir()
	for _, tenant := range []string{"a", "b", "c"} {
		if err := os.Mkdir(filepath.Join(dir, tenant), 0o700); err != nil {
			t.Fatal(err)
		}
		newDB(t, filepath.Join(dir, tenant, "app.db"), tables)
	}
	if err := os.Chmod(filepath.Join(dir, "b"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "b"), 0o700) })
	if paths, _, err := Expand([]string{filepath.Join(dir, "*", "app.db")}); err == nil {
		t.Errorf("Expand() = %q, want an error for the unreadable folder", paths)
	}
}
