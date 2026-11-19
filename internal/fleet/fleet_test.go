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
	if !strings.Contains(failure(results[2]), "no such table") {
		t.Errorf("broken.db failure = %q, want no such table", failure(results[2]))
	}

	again := Run(t.Context(), paths[:2], desired(t), Options{Apply: true, Prune: index.PruneAll}, nil)
	for _, r := range again {
		if failure(r) != "" || !r.Plan.Empty() {
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
	if failure(r) != "" || !r.Plan.Empty() {
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
	if !strings.Contains(failure(r), "hard links") {
		t.Errorf("apply to a hard-linked database: %q, want a refusal", failure(r))
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
		if !strings.Contains(failure(r), "-allow-collations-and-functions") {
			t.Errorf("%s: %q, want a refusal", sql, failure(r))
		}
		if got := indexNames(t, path); !reflect.DeepEqual(got, before) {
			t.Errorf("%s: refused change left indexes %q, want %q", sql, got, before)
		}
		r = Run(t.Context(), []string{path}, want, Options{Apply: true, AllowAppDependent: true, Prune: index.PruneAll}, nil)[0]
		if failure(r) != "" {
			t.Errorf("%s with the flag: %s", sql, failure(r))
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
		isRefused := failure(r) != ""
		if wantRefused := slices.Contains(refused, sql); isRefused != wantRefused {
			t.Errorf("%s: %q, want refused %v", sql, failure(r), wantRefused)
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
	if failure(r) != "" || !slices.Contains(indexNames(t, path), "u") {
		t.Errorf("-prune dropped a UNIQUE index SQLite stored with a trailing comment: %+v", r)
	}

	plain, err := index.Parse("indexes.sql", `CREATE INDEX u ON t (y);`)
	if err != nil {
		t.Fatal(err)
	}
	r = Run(t.Context(), []string{path}, plain, Options{Apply: true, Prune: index.PruneNonUnique}, nil)[0]
	if !strings.Contains(failure(r), "-prune-unique") {
		t.Errorf("replacing UNIQUE u with a plain index: %q, want a refusal", failure(r))
	}
	if r = Run(t.Context(), []string{path}, plain, Options{Apply: true, Prune: index.PruneAll}, nil)[0]; failure(r) != "" {
		t.Errorf("with -prune-unique: %s", failure(r))
	}
}

func TestReplaceOnAnalyzedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	newDB(t, path, tables+`CREATE INDEX orders_status ON orders (status DESC); CREATE INDEX extra ON orders (id, status);
		INSERT INTO orders (customer_id, status) VALUES (1, 'a'), (2, 'b'); ANALYZE;`)
	r := Run(t.Context(), []string{path}, desired(t), Options{Apply: true, Prune: index.PruneNonUnique}, nil)[0]
	if failure(r) != "" {
		t.Errorf("replace and prune on an ANALYZEd database: %s", failure(r))
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

// failure is a result's error and refusals, as one string.
func failure(r Result) string {
	var parts []string
	if r.Err != nil {
		parts = append(parts, r.Err.Error())
	}
	for _, err := range r.Refused {
		parts = append(parts, err.Error())
	}
	return strings.Join(parts, "; ")
}

func TestAllowFlagStillRefusesWhatDiffers(t *testing.T) {
	const schema = `CREATE TABLE t (a TEXT, n TEXT COLLATE NOCASE, g TEXT AS (lower(a)) VIRTUAL, h TEXT AS (a LIKE 'x%') VIRTUAL);
		CREATE TABLE w (k TEXT COLLATE NOCASE PRIMARY KEY, v INT) WITHOUT ROWID;`
	tests := []struct {
		sql               string
		withFlag, without bool // refused?
	}{
		{`CREATE INDEX i ON t (a) WHERE a LIKE 'ab%'`, true, true},
		{`CREATE INDEX i ON t (a) WHERE a GLOB 'ab*'`, true, true},
		{`CREATE INDEX i ON t (a) WHERE a REGEXP '^ab'`, true, true},
		{`CREATE INDEX i ON t (n)`, false, true},
		{`CREATE INDEX i ON t (lower(a))`, false, true},
		{`CREATE INDEX i ON t (g)`, true, true},
		{`CREATE INDEX i ON t (h)`, true, true},
		{`CREATE INDEX i ON w (v)`, false, true},
		{`CREATE INDEX i ON t (a)`, false, false},
	}
	for _, tt := range tests {
		for _, allow := range []bool{true, false} {
			path := filepath.Join(t.TempDir(), "a.db")
			newDB(t, path, schema)
			want, err := index.Parse("indexes.sql", tt.sql)
			if err != nil {
				t.Fatal(err)
			}
			r := Run(t.Context(), []string{path}, want, Options{Apply: true, AllowAppDependent: allow}, nil)[0]
			wantRefused := tt.without
			if allow {
				wantRefused = tt.withFlag
			}
			if got := failure(r) != ""; got != wantRefused {
				t.Errorf("%s, flag %v: %q, want refused %v", tt.sql, allow, failure(r), wantRefused)
			}
		}
	}
}

func TestRefusalsReportedTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	newDB(t, path, `CREATE TABLE t (a, b TEXT COLLATE NOCASE)`)
	want, err := index.Parse("indexes.sql", `CREATE INDEX ok ON t (a); CREATE INDEX nope ON t (missing); CREATE INDEX nc ON t (b);`)
	if err != nil {
		t.Fatal(err)
	}
	for _, apply := range []bool{false, true} {
		r := Run(t.Context(), []string{path}, want, Options{Apply: apply}, nil)[0]
		if len(r.Refused) != 2 {
			t.Errorf("apply %v: refused %v, want nope and nc", apply, r.Refused)
		}
	}
	if got := indexNames(t, path); got != nil {
		t.Errorf("a refused plan changed the database: %q", got)
	}
}

func TestExpandRejectsDoubleStar(t *testing.T) {
	if _, _, err := Expand([]string{"tenants/**/*.db"}); err == nil || !strings.Contains(err.Error(), "**") {
		t.Errorf("Expand(**) = %v, want a refusal", err)
	}
}

func TestStaleNamesAndVersionDrift(t *testing.T) {
	const schema = `CREATE TABLE orders (id INTEGER PRIMARY KEY, customer TEXT, total REAL, x REAL, s TEXT,
			inner_g INT AS (s LIKE 'a%') VIRTUAL, g INT AS (inner_g + 0) VIRTUAL, q INT AS ("like"('a%', s)) VIRTUAL);
		CREATE TABLE pragma_function_list (name TEXT, builtin INT);
		CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT, archived_at TEXT);
		CREATE UNIQUE INDEX users_email ON users (email);
		CREATE TABLE accounts (id INTEGER PRIMARY KEY, email TEXT);`
	tests := []struct {
		sql               string
		without, withFlag bool // refused?
	}{
		{`CREATE INDEX i ON orders (customer || ':' || total)`, true, true},
		{`CREATE INDEX i ON orders (CAST(total AS TEXT))`, true, true},
		{`CREATE INDEX i ON orders (length(total))`, true, true},
		{`CREATE INDEX i ON orders (customer || ':' || s)`, true, true},
		{`CREATE INDEX i ON orders (lower(customer))`, true, false},
		{`CREATE INDEX i ON orders (id) WHERE customer = 1e15`, true, true},
		{`CREATE INDEX i ON orders (id) WHERE total = '5'`, true, true},
		{`CREATE INDEX i ON orders (id) WHERE customer = 'a' AND total > 10`, false, false},
		{`CREATE INDEX i ON orders (lower(total))`, true, true},
		{`CREATE INDEX i ON orders ((x + 1))`, true, true},
		{`CREATE INDEX i ON orders (log10(x))`, true, true},
		{`CREATE INDEX i ON orders (date(s))`, true, true},
		{`CREATE INDEX i ON orders (g)`, true, true},
		{`CREATE INDEX i ON orders (q)`, true, true},
		{`CREATE INDEX i ON orders (s) WHERE s REGEXP '^a'`, true, true},
		{`CREATE UNIQUE INDEX "i" ON "users" ("email") WHERE "deleted_at" IS NULL`, true, true},
		{`CREATE INDEX i ON orders (customer) WHERE customer IS NOT NULL AND total > -1`, false, false},
	}
	for _, tt := range tests {
		for _, allow := range []bool{false, true} {
			path := filepath.Join(t.TempDir(), "a.db")
			newDB(t, path, schema)
			want, err := index.Parse("indexes.sql", tt.sql)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, mustIndex(t, `CREATE UNIQUE INDEX users_email ON users (email)`))
			for _, apply := range []bool{false, true} {
				r := Run(t.Context(), []string{path}, want, Options{Apply: apply, AllowAppDependent: allow}, nil)[0]
				wantRefused := tt.without
				if allow {
					wantRefused = tt.withFlag
				}
				if got := failure(r) != ""; got != wantRefused {
					t.Errorf("%s, flag %v, apply %v: %q, want refused %v", tt.sql, allow, apply, failure(r), wantRefused)
				}
			}
		}
	}

	// A UNIQUE index moved to another table, or changed at all, needs -prune-unique.
	path := filepath.Join(t.TempDir(), "a.db")
	newDB(t, path, schema)
	moved := []index.Index{mustIndex(t, `CREATE UNIQUE INDEX users_email ON accounts (email)`)}
	if r := Run(t.Context(), []string{path}, moved, Options{Apply: true}, nil)[0]; !strings.Contains(failure(r), "-prune-unique") {
		t.Errorf("moving a UNIQUE index: %q, want a refusal", failure(r))
	}
}

func mustIndex(t *testing.T, sql string) index.Index {
	t.Helper()
	ixs, err := index.Parse("indexes.sql", sql)
	if err != nil || len(ixs) != 1 {
		t.Fatalf("Parse(%q) = %v, %v", sql, ixs, err)
	}
	return ixs[0]
}

func TestDataDependentRefusals(t *testing.T) {
	tests := []struct {
		name, setup, sql string
		refused          bool
	}{
		{"nul text in a number column", `CREATE TABLE t (id INTEGER PRIMARY KEY, x INTEGER, b TEXT); INSERT INTO t VALUES (1, 'abc', 'x'), (2, 7, 'y');`,
			`CREATE UNIQUE INDEX ix ON t (b) WHERE x > 5`, true},
		{"numbers only", `CREATE TABLE t (id INTEGER PRIMARY KEY, x INTEGER, b TEXT); INSERT INTO t VALUES (1, 5, 'x'), (2, 7, 'y');`,
			`CREATE UNIQUE INDEX ix ON t (b) WHERE x > 5`, false},
		{"extreme default", `CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT); INSERT INTO t VALUES (1, 'x'); ALTER TABLE t ADD COLUMN eps REAL DEFAULT 1e-300;`,
			`CREATE UNIQUE INDEX ix ON t (eps, a)`, true},
		{"text default on a number column", `CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT); ALTER TABLE t ADD COLUMN n NUMERIC DEFAULT '-1.1637124187458781e-153';`,
			`CREATE INDEX ix ON t (n)`, true},
		{"everyday defaults", `CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT DEFAULT 'none', r REAL DEFAULT 0.0, n INT DEFAULT -1);`,
			`CREATE INDEX ix ON t (a, r, n)`, false},
		{"utf-16 text literal", `PRAGMA encoding = 'UTF-16le'; CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT, b TEXT);`,
			`CREATE UNIQUE INDEX ix ON t (b) WHERE a = 'x'`, true},
		{"utf-16 plain column", `PRAGMA encoding = 'UTF-16le'; CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT, b TEXT);`,
			`CREATE INDEX ix ON t (a, b) WHERE b IS NOT NULL`, false},
		{"column named true", `CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT, b TEXT, "true" REAL);`,
			`CREATE UNIQUE INDEX ix ON t (b) WHERE a = TRUE`, true},
	}
	for _, tt := range tests {
		path := filepath.Join(t.TempDir(), "a.db")
		newDB(t, path, tt.setup)
		want, err := index.Parse("indexes.sql", tt.sql)
		if err != nil {
			t.Fatal(err)
		}
		r := Run(t.Context(), []string{path}, want, Options{Apply: true, AllowAppDependent: true}, nil)[0]
		if got := failure(r) != ""; got != tt.refused {
			t.Errorf("%s: %q, want refused %v", tt.name, failure(r), tt.refused)
		}
	}
	// lower() in a UTF-16 database, even with the flag.
	path := filepath.Join(t.TempDir(), "a.db")
	newDB(t, path, `PRAGMA encoding = 'UTF-16le'; CREATE TABLE t (a TEXT);`)
	if r := Run(t.Context(), []string{path}, []index.Index{mustIndex(t, `CREATE INDEX ix ON t (lower(a))`)}, Options{Apply: true, AllowAppDependent: true}, nil)[0]; failure(r) == "" {
		t.Error("lower() in a UTF-16 database was built")
	}
}
