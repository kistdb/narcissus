package check

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kistdb/narcissus/internal/scratch"
)

const schema = `
CREATE TABLE customers (id INTEGER PRIMARY KEY, email TEXT UNIQUE, name TEXT);
CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INT, status TEXT, created_at TEXT, total REAL);
CREATE TABLE "order items" ("order" INT, sku TEXT);
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);
CREATE TABLE users_email (x);
CREATE TABLE narcissus_candidate (x);
CREATE TABLE regions (id INTEGER PRIMARY KEY, country TEXT);
CREATE TABLE shops (id INTEGER PRIMARY KEY, region_id INT, name TEXT);
CREATE VIEW open_orders AS SELECT * FROM orders WHERE status = 'open' GROUP BY customer_id;
`

const indexes = `
CREATE INDEX orders_status ON orders (status);
CREATE INDEX orders_total ON orders (total);
CREATE UNIQUE INDEX customers_name ON customers (name);
CREATE INDEX users_lower_email ON users (lower(email));
CREATE INDEX "items by sku" ON "order items" (sku);
`

func build(t *testing.T) *scratch.DB {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"schema.sql": schema, "indexes.sql": indexes} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := scratch.Build(t.Context(), filepath.Join(dir, "schema.sql"), filepath.Join(dir, "indexes.sql"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestParseQueries(t *testing.T) {
	qs, err := ParseQueries("queries.sql", `-- Queries for the shop.

-- name: recent_orders
SELECT * FROM orders WHERE customer_id = ?
ORDER BY created_at DESC LIMIT 20;

-- name: export
-- narcissus: allow full-scan, temp-btree
SELECT * FROM orders ORDER BY total;

SELECT 1;
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []Query{
		{Name: "recent_orders", File: "queries.sql", Line: 4, SQL: "SELECT * FROM orders WHERE customer_id = ? ORDER BY created_at DESC LIMIT 20"},
		{Name: "export", File: "queries.sql", Line: 9, SQL: "SELECT * FROM orders ORDER BY total", Allow: []Rule{FullScan, TempBTree}},
		{File: "queries.sql", Line: 11, SQL: "SELECT 1"},
	}
	if !reflect.DeepEqual(qs, want) {
		t.Errorf("ParseQueries() =\n%+v\nwant\n%+v", qs, want)
	}
}

func TestParseQueriesErrors(t *testing.T) {
	tests := []struct{ src, wantErr string }{
		{"-- narcissus: allow slow-scan\nSELECT 1", `queries.sql:2: unknown rule "slow-scan"`},
		{"-- narcissus: skip\nSELECT 1", `unknown directive "skip"`},
		{"-- name: a\nSELECT 1;\n-- name: a\nSELECT 2", "queries.sql:4: query a already named on line 2"},
		{"SELECT 'x", "queries.sql:1: unterminated"},
	}
	for _, tt := range tests {
		_, err := ParseQueries("queries.sql", tt.src)
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("ParseQueries(%q) error = %v, want %q", tt.src, err, tt.wantErr)
		}
	}
}

func TestRun(t *testing.T) {
	db := build(t)
	qs, err := ParseQueries("queries.sql", `
-- name: by_status
SELECT * FROM orders WHERE status = ?;
-- name: recent
SELECT * FROM orders WHERE customer_id = ? ORDER BY created_at DESC LIMIT 20;
-- name: join
SELECT o.* FROM orders o JOIN customers c ON c.id = o.customer_id WHERE c.email = ?;
-- name: export
-- narcissus: allow full-scan
SELECT * FROM orders;
-- name: grouped
SELECT status, count(*) FROM orders GROUP BY status;
-- name: cte
WITH t AS MATERIALIZED (SELECT id FROM orders WHERE status = 'x') SELECT * FROM t;
-- name: items
SELECT * FROM "order items" WHERE "order" = ?;
-- name: broken
SELECT nope FROM orders;
-- name: aliased_cte
WITH recent AS MATERIALIZED (SELECT * FROM orders WHERE status = ?) SELECT * FROM recent AS r;
-- name: aliased_view
SELECT * FROM open_orders AS v;
-- name: values
INSERT INTO orders (customer_id) VALUES (1), (2), (3);
-- name: aliased_table
SELECT * FROM orders AS o WHERE o.customer_id = ?;
-- name: name_clash
SELECT id FROM users WHERE email = ?;
-- name: covering
SELECT email FROM users;
-- name: by_sku
SELECT * FROM "order items" WHERE sku = ?;
-- name: left_join
SELECT c.* FROM customers c LEFT JOIN orders o ON o.status = c.name;
-- name: exists
SELECT * FROM customers c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.total = c.id);
-- name: qualified
SELECT * FROM main.orders WHERE created_at = ?;
-- name: classic_join
SELECT s.name FROM regions r JOIN shops s ON s.region_id = r.id WHERE r.country = ?;
`)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(db, qs)
	if err != nil {
		t.Fatal(err)
	}

	type got struct {
		Problems []Rule
		Suggest  []string
		Err      bool
	}
	gots := make(map[string]got)
	for _, r := range rep.Results {
		g := got{Suggest: r.Suggest, Err: r.Err != nil}
		for _, p := range r.Problems {
			g.Problems = append(g.Problems, p.Rule)
		}
		gots[r.Query.Name] = g
	}
	want := map[string]got{
		"by_status": {},
		"recent": {
			Problems: []Rule{FullScan, TempBTree},
			Suggest:  []string{"CREATE INDEX orders_customer_id_created_at ON orders (customer_id, created_at);"},
		},
		"join": {
			Problems: []Rule{FullScan},
			Suggest:  []string{"CREATE INDEX orders_customer_id ON orders (customer_id);"},
		},
		"export":  {},
		"grouped": {},
		"cte":     {},
		"items": {
			Problems: []Rule{FullScan},
			Suggest:  []string{`CREATE INDEX order_items_order ON "order items" ("order");`},
		},
		"broken":       {Err: true},
		"aliased_cte":  {},
		"aliased_view": {Problems: []Rule{TempBTree}, Suggest: []string{"CREATE INDEX orders_status_customer_id ON orders (status, customer_id);"}},
		"values":       {},
		"aliased_table": {
			Problems: []Rule{FullScan},
			Suggest:  []string{"CREATE INDEX orders_customer_id ON orders (customer_id);"},
		},
		"name_clash": {
			Problems: []Rule{FullScan},
			Suggest:  []string{"CREATE INDEX users_email_2 ON users (email);"},
		},
		"covering":  {Problems: []Rule{FullScan}},
		"by_sku":    {},
		"left_join": {Problems: []Rule{FullScan}},
		"exists":    {Problems: []Rule{FullScan}},
		"qualified": {Problems: []Rule{FullScan}, Suggest: []string{"CREATE INDEX orders_created_at ON orders (created_at);"}},
		"classic_join": {
			Problems: []Rule{FullScan},
			Suggest:  []string{"CREATE INDEX regions_country ON regions (country);", "CREATE INDEX shops_region_id ON shops (region_id);"},
		},
	}
	if !reflect.DeepEqual(gots, want) {
		t.Errorf("results:\n%+v\nwant\n%+v", gots, want)
	}
	if !rep.Failed() {
		t.Error("Failed() = false, want true")
	}
	var unused []string
	for _, ix := range rep.Unused {
		unused = append(unused, ix.Name)
		if ix.File == "" {
			t.Errorf("unused index %s has no file", ix.Name)
		}
	}
	if want := []string{"users_lower_email"}; !reflect.DeepEqual(unused, want) {
		t.Errorf("Unused = %q, want %q", unused, want)
	}
}

func TestSuggestLeavesDatabaseUnchanged(t *testing.T) {
	db := build(t)
	qs, err := ParseQueries("queries.sql", "SELECT * FROM orders WHERE customer_id = ?;\nSELECT * FROM orders WHERE customer_id = ?;")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(db, qs)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Results {
		if len(r.Problems) != 1 || len(r.Suggest) != 1 {
			t.Errorf("line %d: problems %v, suggest %v; want one each, every time", r.Query.Line, r.Problems, r.Suggest)
		}
	}
}

func TestAnalyze(t *testing.T) {
	tg := target{
		Query:   Query{Allow: []Rule{TempBTree}},
		aliases: map[string]string{"o": "orders", "r": "recent"},
	}
	steps := []string{
		"CO-ROUTINE (subquery-1)",
		"MATERIALIZE recent",
		"SCAN orders",
		"SCAN O",
		"SCAN o LEFT-JOIN",
		"SCAN o EXISTS",
		"SCAN main.orders",
		"USE TEMP B-TREE FOR GROUP BY",
		"SCAN (subquery-1)",
		"SCAN r",
		"SCAN CONSTANT ROW",
		"SCAN 3-ROW VALUES CLAUSE",
		"SCAN orders USING INDEX orders_created",
		"SCAN docs VIRTUAL TABLE INDEX 0:M1",
		"SEARCH c USING AUTOMATIC COVERING INDEX (id=?)",
		"SEARCH AUTOMATIC_RENEWALS USING COVERING INDEX ar_user (user_id=?)",
	}
	ps, indexScans := tg.analyze(steps)
	want := []Problem{
		{FullScan, "SCAN orders"},
		{FullScan, "SCAN O"},
		{FullScan, "SCAN o LEFT-JOIN"},
		{FullScan, "SCAN o EXISTS"},
		{FullScan, "SCAN main.orders"},
		{AutomaticIndex, "SEARCH c USING AUTOMATIC COVERING INDEX (id=?)"},
	}
	if !reflect.DeepEqual(ps, want) || indexScans != 1 {
		t.Errorf("analyze() = %+v, %d\nwant %+v, 1", ps, indexScans, want)
	}
}

func TestUsesIndex(t *testing.T) {
	tests := []struct {
		step, name string
		want       bool
	}{
		{"SEARCH t USING INDEX t by a (a=?)", "t by a", true},
		{"SEARCH t USING COVERING INDEX T_A", "t_a", true},
		{"SEARCH t USING INDEX t_ab (a=?)", "t_a", false},
		{"SCAN t", "t", false},
	}
	for _, tt := range tests {
		if got := usesIndex(tt.step, tt.name); got != tt.want {
			t.Errorf("usesIndex(%q, %q) = %v, want %v", tt.step, tt.name, got, tt.want)
		}
	}
}

func TestUnusedExemptions(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"schema.sql": `CREATE TABLE parent (id INTEGER PRIMARY KEY);
			CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INT REFERENCES parent (id), note TEXT, tag TEXT);`,
		"indexes.sql": `CREATE INDEX child_parent ON child (parent_id);
-- Narcissus: allow unused
CREATE INDEX child_note ON child (note);
CREATE INDEX child_tag ON child (tag);`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := scratch.Build(t.Context(), filepath.Join(dir, "schema.sql"), filepath.Join(dir, "indexes.sql"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	qs, err := ParseQueries("queries.sql", "-- NARCISSUS: Allow Full-Scan\nSELECT * FROM child;")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(db, qs)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed() {
		t.Errorf("an allowed scan failed: %+v", rep.Results)
	}
	if len(rep.Unused) != 1 || rep.Unused[0].Name != "child_tag" {
		t.Errorf("Unused = %+v, want only child_tag", rep.Unused)
	}
}
