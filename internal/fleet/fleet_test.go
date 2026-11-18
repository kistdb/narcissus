package fleet

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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
	c, err := sqlite.Open(t.Context(), path, false)
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
	plans := Run(t.Context(), paths, desired(t), Options{Jobs: 2})
	if plans[0].Err != nil || plans[1].Err != nil || plans[2].Err != nil {
		t.Fatalf("plan errors: %v, %v, %v", plans[0].Err, plans[1].Err, plans[2].Err)
	}
	if n := len(plans[1].Plan.Changes); n != 2 || len(plans[1].Plan.Unmanaged) != 1 {
		t.Errorf("stale plan = %+v, want 2 changes and 1 unmanaged", plans[1].Plan)
	}
	if got := indexNames(t, fresh); got != nil {
		t.Errorf("plan changed fresh.db: %q", got)
	}

	results := Run(t.Context(), paths, desired(t), Options{Apply: true, Prune: true, Jobs: 2, BusyTimeout: time.Second})
	for i, want := range [][]string{{"orders_customer", "orders_status"}, {"orders_customer", "orders_status"}, nil} {
		if got := indexNames(t, paths[i]); !reflect.DeepEqual(sorted(got), want) {
			t.Errorf("%s indexes = %q, want %q", filepath.Base(paths[i]), got, want)
		}
	}
	if results[2].Err == nil || !strings.Contains(results[2].Err.Error(), "no such table") {
		t.Errorf("broken.db error = %v, want no such table", results[2].Err)
	}

	again := Run(t.Context(), paths[:2], desired(t), Options{Apply: true, Prune: true})
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
	for _, r := range Run(ctx, []string{path, path}, desired(t), Options{Apply: true, Jobs: 1}) {
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

	got, err := Expand([]string{b, filepath.Join(dir, "*.db*"), a})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{b, a}; !reflect.DeepEqual(got, want) {
		t.Errorf("Expand() = %q, want %q", got, want)
	}
	literal := filepath.Join(dir, "t[1].sqlite3")
	if got, err := Expand([]string{literal}); err != nil || !reflect.DeepEqual(got, []string{literal}) {
		t.Errorf("Expand(%q) = %q, %v; want the file itself", literal, got, err)
	}

	for _, args := range [][]string{
		{filepath.Join(dir, "missing.db")},
		{filepath.Join(dir, "*.sqlite")},
		{filepath.Join(dir, "sub.db")},
		{filepath.Join(dir, "[")},
	} {
		if _, err := Expand(args); err == nil {
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

	r := Run(t.Context(), []string{path}, desired(t), Options{Apply: true})[0]
	if r.Err != nil || !r.Plan.Empty() {
		t.Errorf("apply to an in-sync database with a writer: %+v", r)
	}
}
