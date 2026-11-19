package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kistdb/narcissus/internal/index"
)

func TestInit(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "app.db")
	newDB(t, db, `
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT UNIQUE, deleted_at TEXT);
CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INT REFERENCES users, total REAL);
CREATE INDEX orders_user ON orders (user_id);
CREATE INDEX users_live
  ON users (email) WHERE deleted_at IS NULL;
CREATE VIRTUAL TABLE docs USING fts5(body);
CREATE VIEW big_orders AS SELECT * FROM orders WHERE total > 100;
`)
	var out bytes.Buffer
	if err := run(t.Context(), []string{"init", "-dir", dir, db}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "4 tables, views and triggers; 2 indexes.") {
		t.Errorf("output:\n%s", out.String())
	}
	got, err := os.ReadFile(filepath.Join(dir, "indexes.sql"))
	if err != nil {
		t.Fatal(err)
	}
	want := "-- Written by narcissus init from app.db.\n\n" +
		"CREATE INDEX orders_user ON orders (user_id);\n" +
		"CREATE INDEX users_live ON users (email) WHERE deleted_at IS NULL;\n"
	if string(got) != want {
		t.Errorf("indexes.sql:\n%s\nwant:\n%s", got, want)
	}

	// The written files rebuild the schema, and the database already matches them.
	if err := os.WriteFile(filepath.Join(dir, "queries.sql"), []byte("SELECT * FROM orders WHERE user_id = ?;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(t.Context(), checkArgs(dir), &out, io.Discard); err != nil {
		t.Errorf("check after init: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := run(t.Context(), []string{"plan", "-indexes", filepath.Join(dir, "indexes.sql"), db}, &out, io.Discard); err != nil || !strings.Contains(out.String(), "0 to change, 1 in sync") {
		t.Errorf("plan after init: %v\n%s", err, out.String())
	}

	if err := os.Remove(filepath.Join(dir, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	err = run(t.Context(), []string{"init", "-dir", dir, db}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "indexes.sql already exists (-force overwrites it)") {
		t.Errorf("second init error = %v, want already exists", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "schema.sql")); err == nil {
		t.Error("init wrote schema.sql before failing on indexes.sql")
	}
	out.Reset()
	if err := run(t.Context(), []string{"init", "-dir", dir, "-force", db}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "kept "+filepath.Join(dir, "queries.sql")) {
		t.Errorf("init -force output:\n%s\nwant queries.sql kept", out.String())
	}
	if q, _ := os.ReadFile(filepath.Join(dir, "queries.sql")); !strings.HasPrefix(string(q), "SELECT") {
		t.Errorf("init -force replaced queries.sql:\n%s", q)
	}
}

func TestInitEdgeCases(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "app.db")
	newDB(t, db, "CREATE TABLE t (a);\nCREATE VIEW v AS SELECT a FROM t -- the end\n;")
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"init", "-dir", out, db}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "queries.sql"), []byte("SELECT a FROM v;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"check", "-schema", filepath.Join(out, "schema.sql"), "-indexes", filepath.Join(out, "indexes.sql"), "-queries", filepath.Join(out, "queries.sql")}
	var res bytes.Buffer
	if err := run(t.Context(), args, &res, io.Discard); err != nil && !errors.Is(err, errFailed) {
		t.Errorf("check on init's output: %v\n%s", err, res.String())
	}

	// A dangling symlink counts as an existing file.
	sym := filepath.Join(dir, "sym")
	if err := os.Mkdir(sym, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside.sql")
	if err := os.Symlink(outside, filepath.Join(sym, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	err := run(t.Context(), []string{"init", "-dir", sym, db}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("init through a dangling symlink: %v, want a refusal", err)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("init wrote through the symlink")
	}

	for _, bad := range []string{"empty.db", "notes.db"} {
		p := filepath.Join(dir, bad)
		if err := os.WriteFile(p, []byte(strings.Repeat("x", len(bad)-7)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := run(t.Context(), []string{"init", "-dir", out, "-force", p}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "not a SQLite database") {
			t.Errorf("init %s: %v, want not a SQLite database", bad, err)
		}
	}
}

func TestInitStoredOddities(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "app.db")
	newDB(t, db, "CREATE TABLE t (a, b);\nCREATE INDEX t_a ON t (a) /* by a")
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"init", "-dir", out, db}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(out, "indexes.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := index.Parse("indexes.sql", string(src)); err != nil {
		t.Errorf("init wrote an indexes.sql that doesn't parse: %v\n%s", err, src)
	}

	if err := os.Remove(filepath.Join(out, "indexes.sql")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(out, "indexes.sql"), 0o700); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(out, "schema.sql"))
	err = run(t.Context(), []string{"init", "-dir", out, "-force", db}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "is a folder") {
		t.Errorf("init -force with indexes.sql a folder: %v, want a refusal", err)
	}
	if after, _ := os.ReadFile(filepath.Join(out, "schema.sql")); !bytes.Equal(before, after) {
		t.Error("init replaced schema.sql before failing")
	}
}

func TestInitDirAndPermissions(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "app.db")
	newDB(t, db, "CREATE TABLE t (a);")
	err := run(t.Context(), []string{"init", "-dir", filepath.Join(dir, "nope"), db}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "isn't a folder") {
		t.Errorf("init -dir missing: %v", err)
	}
	schema := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schema, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"init", "-dir", dir, "-force", db}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(schema); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("schema.sql after -force: %v %v, want 0600 kept", fi.Mode().Perm(), err)
	}
}

func TestInitWontOverwriteItsDatabase(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "schema.sql")
	newDB(t, db, "CREATE TABLE t (a); INSERT INTO t VALUES (1);")
	before, _ := os.ReadFile(db)
	err := run(t.Context(), []string{"init", "-dir", dir, "-force", db}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "the database itself") {
		t.Errorf("init -force over its own database: %v", err)
	}
	if after, _ := os.ReadFile(db); !bytes.Equal(before, after) {
		t.Error("init changed the database")
	}
}
