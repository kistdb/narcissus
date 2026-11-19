package scratch

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kistdb/narcissus/internal/sqlite"
)

// migrations returns the up migrations in dir in the order to run them: plain .sql
// files, golang-migrate .up.sql files, up sections, and Prisma's folder/migration.sql.
func migrations(dir string) ([]sqlite.Script, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		switch {
		case e.IsDir():
			if _, err := os.Stat(filepath.Join(dir, n, "migration.sql")); err == nil {
				names = append(names, filepath.Join(n, "migration.sql"))
			}
		case strings.HasSuffix(n, ".sql") && !strings.HasSuffix(n, ".down.sql"):
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s: no .sql migrations", dir)
	}
	slices.SortFunc(names, compareMigrations)

	scripts := make([]sqlite.Script, 0, len(names))
	for _, n := range names {
		path := filepath.Join(dir, n)
		text, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		body, line := upSection(string(text))
		scripts = append(scripts, sqlite.Script{File: path, Line: line, Text: body})
	}
	return scripts, nil
}

// compareMigrations orders file names by the first number in them, then by name.
func compareMigrations(a, b string) int {
	va, vb := version(a), version(b)
	return cmp.Or(cmp.Compare(len(va), len(vb)), cmp.Compare(va, vb), cmp.Compare(a, b))
}

// version returns the first run of digits in name, without leading zeros.
func version(name string) string {
	i := strings.IndexAny(name, "0123456789")
	if i < 0 {
		return ""
	}
	j := i
	for j < len(name) && '0' <= name[j] && name[j] <= '9' {
		j++
	}
	return strings.TrimLeft(name[i:j], "0")
}

// upSection returns the dbmate or goose up section of text and the line it
// starts on, or all of text for a plain migration.
func upSection(text string) (string, int) {
	lines := strings.SplitAfter(text, "\n")
	start := -1
	for i, l := range lines {
		l = strings.TrimSpace(l)
		switch {
		case start < 0 && (l == "-- +goose Up" || l == "-- +migrate Up" || strings.HasPrefix(l, "-- +migrate Up ") ||
			l == "-- migrate:up" || strings.HasPrefix(l, "-- migrate:up ")):
			start = i + 1
		case start >= 0 && (l == "-- +goose Down" || l == "-- +migrate Down" || strings.HasPrefix(l, "-- +migrate Down ") ||
			l == "-- migrate:down" || strings.HasPrefix(l, "-- migrate:down ")):
			return strings.Join(lines[start:i], ""), start + 1
		}
	}
	if start < 0 {
		return text, 1
	}
	return strings.Join(lines[start:], ""), start + 1
}
