package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// project writes files into a temporary directory and returns its path.
func project(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func shop(t *testing.T, queries string) string {
	return project(t, map[string]string{
		"schema.sql":  "CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INT, status TEXT);\nCREATE INDEX legacy ON orders (id, status);\n",
		"indexes.sql": "CREATE INDEX orders_status ON orders (status);\nCREATE INDEX orders_unused ON orders (id, customer_id);\n",
		"queries.sql": queries,
	})
}

func checkArgs(dir string, extra ...string) []string {
	return append([]string{"check",
		"-schema", filepath.Join(dir, "schema.sql"),
		"-indexes", filepath.Join(dir, "indexes.sql"),
		"-queries", filepath.Join(dir, "queries.sql"),
	}, extra...)
}

const failing = "-- name: by_status\nSELECT * FROM orders WHERE status = ?;\n\n-- name: by_customer\nSELECT * FROM orders WHERE customer_id = ?;\n"

func TestCheckText(t *testing.T) {
	dir := shop(t, failing)
	var out bytes.Buffer
	err := run(t.Context(), checkArgs(dir), &out, io.Discard)
	if code := exitCode(err, io.Discard); code != 1 {
		t.Errorf("exit code %d, want 1 (error %v)", code, err)
	}
	q, ix, schema := filepath.Join(dir, "queries.sql"), filepath.Join(dir, "indexes.sql"), filepath.Join(dir, "schema.sql")
	want := q + ":5: by_customer: full-scan: SCAN orders\n" +
		"  suggest: CREATE INDEX orders_customer_id ON orders (customer_id);\n" +
		ix + ":2: warning: index orders_unused is not used by any query\n" +
		schema + ": warning: index legacy is not in the indexes file, so was left out\n" +
		"2 queries checked, 1 failed.\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestCheckPasses(t *testing.T) {
	dir := shop(t, "SELECT * FROM orders WHERE status = ?;\n-- narcissus: allow full-scan\nSELECT * FROM orders WHERE customer_id = ?;\n")
	var out bytes.Buffer
	if err := run(t.Context(), checkArgs(dir), &out, io.Discard); err != nil {
		t.Fatalf("run() = %v\n%s", err, out.String())
	}
	if !strings.HasSuffix(out.String(), "2 queries checked, 0 failed.\n") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestCheckGitHub(t *testing.T) {
	dir := shop(t, failing)
	var out bytes.Buffer
	_ = run(t.Context(), checkArgs(dir, "-format", "github"), &out, io.Discard)
	want := ",line=5,title=narcissus by_customer::full-scan: SCAN orders%0Asuggest: CREATE INDEX orders_customer_id"
	if !strings.HasPrefix(out.String(), "::error file=") || !strings.Contains(out.String(), want) {
		t.Errorf("output:\n%s\nwant it to contain:\n%s", out.String(), want)
	}
	if !strings.Contains(out.String(), "::warning file=") {
		t.Errorf("output has no warnings:\n%s", out.String())
	}
}

func TestCheckJSON(t *testing.T) {
	dir := shop(t, failing)
	var out bytes.Buffer
	_ = run(t.Context(), checkArgs(dir, "-format", "json"), &out, io.Discard)
	var got struct {
		Failed  bool
		Queries []struct {
			Name     string
			Problems []struct{ Rule, Detail string }
			Suggest  []string
		}
		Unused []struct{ Name string } `json:"unused_indexes"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !got.Failed || len(got.Queries) != 2 || len(got.Queries[1].Problems) != 1 || len(got.Queries[1].Suggest) != 1 ||
		len(got.Unused) != 1 || got.Unused[0].Name != "orders_unused" {
		t.Errorf("unexpected report:\n%s", out.String())
	}
}

func TestCheckErrors(t *testing.T) {
	dir := shop(t, "SELECT 1;")
	tests := []struct {
		name     string
		args     []string
		wantErr  string
		wantCode int
	}{
		{"bad format", checkArgs(dir, "-format", "xml"), `unknown format "xml"`, 2},
		{"extra argument", append(checkArgs(dir), "app.db"), "doesn't take a database", 2},
		{"missing queries", []string{"check", "-schema", filepath.Join(dir, "schema.sql"), "-indexes", filepath.Join(dir, "indexes.sql"), "-queries", filepath.Join(dir, "nope.sql")}, "no such file", 1},
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
