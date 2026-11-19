package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3"
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

func TestPlanAndApply(t *testing.T) {
	dir := project(t, map[string]string{
		"indexes.sql": "CREATE INDEX orders_customer ON orders (customer_id);\n",
	})
	const tables = "CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INT);"
	newDB(t, filepath.Join(dir, "a.db"), tables)
	newDB(t, filepath.Join(dir, "b.db"), tables+"CREATE INDEX orders_customer ON orders (customer_id); CREATE INDEX legacy ON orders (id);")
	newDB(t, filepath.Join(dir, "c.db"), "CREATE TABLE other (x);")
	indexes := filepath.Join(dir, "indexes.sql")
	glob := filepath.Join(dir, "*.db")
	a, b, c := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db"), filepath.Join(dir, "c.db")

	var out bytes.Buffer
	err := run(t.Context(), []string{"plan", "-indexes", indexes, glob}, &out, io.Discard)
	if code := exitCode(err, io.Discard); code != 1 {
		t.Errorf("plan exit code %d, want 1: c.db has no orders table", code)
	}
	want := a + "\n  + CREATE INDEX orders_customer ON orders (customer_id)\n" +
		b + "\n  ! keeping legacy, which " + indexes + " doesn't declare (-prune drops it)\n" +
		c + "\n  + CREATE INDEX orders_customer ON orders (customer_id)\n" +
		"  ! orders_customer: no such table: main.orders\n" +
		"  Nothing was changed in this database.\n" +
		"3 databases: 1 to change, 1 in sync, 1 failed.\n"
	if out.String() != want {
		t.Errorf("plan output:\n%s\nwant:\n%s", out.String(), want)
	}

	out.Reset()
	err = run(t.Context(), []string{"apply", "-indexes", indexes, "-prune", "-jobs", "2", glob}, &out, io.Discard)
	if code := exitCode(err, io.Discard); code != 1 {
		t.Errorf("apply exit code %d, want 1 for c.db", code)
	}
	for _, line := range []string{
		b + "\n  - DROP INDEX legacy\n",
		"  ! orders_customer: no such table: main.orders\n",
		"3 databases: 2 changed, 0 in sync, 1 failed.\n",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("apply output:\n%s\nwant it to contain:\n%s", out.String(), line)
		}
	}

	out.Reset()
	if err := run(t.Context(), []string{"plan", "-indexes", indexes, "-prune", a, b}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if out.String() != "2 databases: 0 to change, 2 in sync, 0 failed.\n" {
		t.Errorf("plan after apply:\n%s", out.String())
	}
}

func TestSyncErrors(t *testing.T) {
	dir := project(t, map[string]string{"indexes.sql": "CREATE INDEX i ON t (a);\n", "bad.sql": "DROP TABLE t;\n"})
	tests := []struct {
		name     string
		args     []string
		wantErr  string
		wantCode int
	}{
		{"no databases", []string{"apply", "-indexes", filepath.Join(dir, "indexes.sql")}, "apply: please give one or more databases", 2},
		{"bad jobs", []string{"plan", "-jobs", "0", "x.db"}, "-jobs must be at least 1", 2},
		{"no match", []string{"plan", "-indexes", filepath.Join(dir, "indexes.sql"), filepath.Join(dir, "*.db")}, "no databases match", 1},
		{"bad indexes", []string{"plan", "-indexes", filepath.Join(dir, "bad.sql"), "x.db"}, "bad.sql:1: want only CREATE INDEX", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(t.Context(), tt.args, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("run() error = %v, want %q", err, tt.wantErr)
			}
			if code := exitCode(err, io.Discard); code != tt.wantCode {
				t.Errorf("exit code %d, want %d", code, tt.wantCode)
			}
		})
	}
}

func TestPlanExitCode(t *testing.T) {
	dir := project(t, map[string]string{"indexes.sql": "CREATE INDEX orders_customer ON orders (customer_id);\n"})
	db := filepath.Join(dir, "app.db")
	newDB(t, db, "CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INT);")
	indexes := filepath.Join(dir, "indexes.sql")

	err := run(t.Context(), []string{"plan", "-exit-code", "-indexes", indexes, db}, io.Discard, io.Discard)
	if code := exitCode(err, io.Discard); code != 3 {
		t.Errorf("plan -exit-code with changes pending: exit %d, want 3", code)
	}
	if err := run(t.Context(), []string{"plan", "-indexes", indexes, db}, io.Discard, io.Discard); err != nil {
		t.Errorf("plan without -exit-code: %v, want success", err)
	}
	if err := run(t.Context(), []string{"apply", "-indexes", indexes, db}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"plan", "-exit-code", "-indexes", indexes, db}, io.Discard, io.Discard); err != nil {
		t.Errorf("plan -exit-code in sync: %v, want success", err)
	}
	err = run(t.Context(), []string{"apply", "-exit-code", db}, io.Discard, io.Discard)
	if code := exitCode(err, io.Discard); code != 2 {
		t.Errorf("apply -exit-code: exit %d, want 2 (plan only)", code)
	}
}

func TestPruneGuards(t *testing.T) {
	dir := project(t, map[string]string{"empty.sql": "-- nothing yet\n", "one.sql": "CREATE INDEX i ON t (a);\n"})
	db := filepath.Join(dir, "app.db")
	newDB(t, db, "CREATE TABLE t (a, b); CREATE INDEX t_b ON t (b); CREATE UNIQUE INDEX t_u ON t (a, b);")

	err := run(t.Context(), []string{"apply", "-prune", "-indexes", filepath.Join(dir, "empty.sql"), db}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "doesn't declare any indexes") {
		t.Errorf("apply -prune with an empty indexes file: %v, want a refusal", err)
	}
	err = run(t.Context(), []string{"apply", "-prune-unique", "-indexes", filepath.Join(dir, "one.sql"), db}, io.Discard, io.Discard)
	if code := exitCode(err, io.Discard); code != 2 {
		t.Errorf("-prune-unique without -prune: exit %d, want 2", code)
	}

	var out bytes.Buffer
	if err := run(t.Context(), []string{"apply", "-prune", "-indexes", filepath.Join(dir, "one.sql"), db}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"  - DROP INDEX t_b\n", "  ! keeping t_u, which " + filepath.Join(dir, "one.sql") + " doesn't declare (it's UNIQUE: -prune -prune-unique drops it)\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output:\n%s\nwant it to contain:\n%s", out.String(), want)
		}
	}
}

func TestPlanFlagsAppDependent(t *testing.T) {
	dir := project(t, map[string]string{"indexes.sql": "CREATE INDEX t_lower ON t (lower(a));\n"})
	db := filepath.Join(dir, "app.db")
	newDB(t, db, "CREATE TABLE t (a TEXT);")
	var out bytes.Buffer
	err := run(t.Context(), []string{"plan", "-indexes", filepath.Join(dir, "indexes.sql"), db}, &out, io.Discard)
	if code := exitCode(err, io.Discard); code != 1 {
		t.Errorf("plan exit code %d, want 1", code)
	}
	if !strings.Contains(out.String(), "  ! t_lower: depends on lower(a)") || !strings.Contains(out.String(), "-allow-collations-and-functions") {
		t.Errorf("plan output:\n%s", out.String())
	}
	out.Reset()
	if err := run(t.Context(), []string{"plan", "-allow-collations-and-functions", "-indexes", filepath.Join(dir, "indexes.sql"), db}, &out, io.Discard); err != nil {
		t.Errorf("plan with the flag: %v\n%s", err, out.String())
	}
}

func TestOutputQuotesControlCharacters(t *testing.T) {
	dir := project(t, map[string]string{"indexes.sql": "CREATE INDEX \"evil\n+ fake\" ON t (a);\n"})
	db := filepath.Join(dir, "app.db")
	newDB(t, db, "CREATE TABLE t (a);")
	var out bytes.Buffer
	if err := run(t.Context(), []string{"plan", "-indexes", filepath.Join(dir, "indexes.sql"), db}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\n+ fake") {
		t.Errorf("a name with a newline forged an output line:\n%s", out.String())
	}
}

func TestFlagsAfterNamesAndInterruption(t *testing.T) {
	dir := project(t, map[string]string{"indexes.sql": "CREATE INDEX i ON t (a);\n"})
	db := filepath.Join(dir, "app.db")
	newDB(t, db, "CREATE TABLE t (a);")
	err := run(t.Context(), []string{"plan", "-indexes", filepath.Join(dir, "indexes.sql"), db, "-prune"}, io.Discard, io.Discard)
	if code := exitCode(err, io.Discard); code != 2 || !strings.Contains(err.Error(), "flags go before") {
		t.Errorf("flag after a name: exit %d, %v", code, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	err = run(ctx, []string{"apply", "-indexes", filepath.Join(dir, "indexes.sql"), db, db}, &out, io.Discard)
	if exitCode(err, io.Discard) != 1 || strings.Contains(out.String(), "context canceled") && !strings.Contains(out.String(), "not started") {
		t.Errorf("interrupted run: %v\n%s", err, out.String())
	}
}
