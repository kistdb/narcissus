package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3"
)

func localizedDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.db")
	c, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.CreateCollation("LOCALIZED", func(a, b []byte) int { return bytes.Compare(b, a) }); err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(`CREATE TABLE people (id INTEGER PRIMARY KEY, name TEXT COLLATE LOCALIZED UNIQUE);
		INSERT INTO people (name) VALUES ('Émile'), ('zoe');`); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCollations(t *testing.T) {
	db := localizedDB(t)
	plain := filepath.Join(t.TempDir(), "plain.db")
	newDB(t, plain, "CREATE TABLE t (a TEXT COLLATE NOCASE);")

	var out bytes.Buffer
	err := run(t.Context(), []string{"collations", db, plain}, &out, io.Discard)
	if code := exitCode(err, io.Discard); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	for _, want := range []string{"table people, column name: LOCALIZED", plain + ": only SQLite's built-in collations."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output:\n%s\nwant %q", out.String(), want)
		}
	}
	out.Reset()
	_ = run(t.Context(), []string{"collations", "-format", "json", db}, &out, io.Discard)
	var got []struct {
		Uses []struct{ Collation string }
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got) != 1 || len(got[0].Uses) == 0 {
		t.Errorf("json: %v\n%s", err, out.String())
	}
	if err := run(t.Context(), []string{"collations", plain}, io.Discard, io.Discard); err != nil {
		t.Errorf("a database with built-ins only: %v", err)
	}
}

func TestConvertCommand(t *testing.T) {
	db := localizedDB(t)
	before, _ := os.ReadFile(db)
	out := filepath.Join(t.TempDir(), "out.db")

	err := run(t.Context(), []string{"convert", db, out}, io.Discard, io.Discard)
	if code := exitCode(err, io.Discard); code != 1 {
		t.Errorf("without a mapping: exit %d, want 1", code)
	}
	var buf bytes.Buffer
	if err := run(t.Context(), []string{"convert", "-to", "nocase", db, out}, &buf, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "dry run") {
		t.Errorf("output:\n%s", buf.String())
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a dry run wrote the file")
	}
	if err := run(t.Context(), []string{"convert", "-to", "nocase", "-write", db, out}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(db); !bytes.Equal(before, after) {
		t.Error("convert changed the original database")
	}
	if err := run(t.Context(), []string{"collations", out}, io.Discard, io.Discard); err != nil {
		t.Errorf("the converted file still has other collations: %v", err)
	}
	err = run(t.Context(), []string{"convert", "-to", "nocase", "-write", db, out}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("writing over an existing file: %v", err)
	}
	err = run(t.Context(), []string{"convert", "-to", "icu", db, out + "2"}, io.Discard, io.Discard)
	if code := exitCode(err, io.Discard); code != 2 {
		t.Errorf("-to icu: exit %d, want 2", code)
	}
}
