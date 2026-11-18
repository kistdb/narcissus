package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
