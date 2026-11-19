package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/kistdb/narcissus/internal/check"
	"github.com/kistdb/narcissus/internal/collate"
	"github.com/kistdb/narcissus/internal/sqlite"
	"github.com/kistdb/narcissus/internal/sqltext"
)

// runCollations lists collations other SQLite programs don't have. It only reads.
func runCollations(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := newFlagSet("collations", stderr)
	format := flags.String("format", "text", "output `format`: text or json")
	if err := parse(flags, args, -1); err != nil {
		return err
	}
	if *format != "text" && *format != "json" {
		return usageError{err: fmt.Errorf("collations: unknown format %q", *format)}
	}
	type result struct {
		Path      string             `json:"path"`
		Uses      []collate.Use      `json:"uses"`
		Overrides []collate.Override `json:"nocase_overrides"`
		Unchecked []string           `json:"unchecked_tables"`
		Error     string             `json:"error,omitempty"`
	}
	var results []result
	found, uses := false, false
	for _, path := range flags.Args() {
		r := result{Path: path, Uses: []collate.Use{}, Overrides: []collate.Override{}, Unchecked: []string{}}
		rep, err := findIn(ctx, path)
		if err != nil {
			r.Error = message(err)
			found = true
		} else {
			r.Uses, r.Overrides, r.Unchecked = append(r.Uses, rep.Uses...), append(r.Overrides, rep.Overrides...), append(r.Unchecked, rep.Unchecked...)
			found = found || rep.Found()
			uses = uses || len(rep.Uses) > 0
		}
		results = append(results, r)
	}
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return err
		}
	} else {
		for _, r := range results {
			switch {
			case r.Error != "":
				fmt.Fprintf(stdout, "%s: error: %s\n", clean(r.Path), r.Error)
				continue
			case len(r.Uses) == 0 && len(r.Overrides) == 0:
				fmt.Fprintf(stdout, "%s: only SQLite's built-in collations.\n", clean(r.Path))
			default:
				fmt.Fprintln(stdout, clean(r.Path))
			}
			for _, u := range r.Uses {
				fmt.Fprintf(stdout, "  %s\n", clean(u.String()))
			}
			for _, o := range r.Overrides {
				fmt.Fprintf(stdout, "  ! index %s isn't in the order SQLite's own collation gives (%s); the app that built it may have replaced that collation\n", clean(o.Index), clean(o.Problem))
			}
			for _, t := range r.Unchecked {
				fmt.Fprintf(stdout, "  ? the NOCASE and RTRIM indexes on %s couldn't be checked, as the table uses a collation or function this SQLite doesn't have\n", clean(t))
			}
		}
		if uses {
			fmt.Fprintln(stdout, "Other SQLite programs can't compare or sort by collations they don't have. narcissus convert can write a copy that uses built-in ones.")
		}
		if slices.ContainsFunc(results, func(r result) bool { return len(r.Overrides) > 0 }) {
			fmt.Fprintln(stdout, "Other SQLite programs that write to a database marked ! could put its indexes out of order.")
		}
	}
	if found {
		return errFailed
	}
	return nil
}

func findIn(ctx context.Context, path string) (collate.Report, error) {
	c, err := sqlite.Open(ctx, path, sqlite.Options{BusyTimeout: sqlite.DefaultBusyTimeout})
	if err != nil {
		return collate.Report{}, err
	}
	defer c.Close()
	return collate.Find(c)
}

// runConvert plans, and with -write makes, a copy of a database using built-in collations.
func runConvert(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := newFlagSet("convert", stderr)
	to := flags.String("to", "", "the built-in `collation` (binary, nocase or rtrim) for any not named in -map")
	pairs := flags.String("map", "", "`NAME=collation` pairs, comma-separated, such as LOCALIZED=nocase,UNICODE=binary")
	queries := flags.String("queries", "", "a queries `file` to point out queries the change affects")
	write := flags.Bool("write", false, "write the new file; without it, only show what would happen")
	if err := parse(flags, args, 2); err != nil {
		return err
	}
	m, err := collate.ParseMapping(*to, *pairs)
	if err != nil {
		return usageError{err: err}
	}
	src, out := flags.Arg(0), flags.Arg(1)
	if src == "" || out == "" {
		return usageError{err: errors.New("convert: please name the database and the new file")}
	}
	if _, err := os.Lstat(out); err == nil {
		return fmt.Errorf("%s already exists; please choose a new file", out)
	}
	for _, side := range []string{"-journal", "-wal", "-shm"} {
		if strings.HasSuffix(strings.ToLower(out), side) {
			return fmt.Errorf("%s would look like a database's %s file to SQLite; please choose another name", out, side)
		}
	}
	c, err := sqlite.Open(ctx, src, sqlite.Options{BusyTimeout: sqlite.DefaultBusyTimeout})
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	defer c.Close()
	rep, err := collate.Begin(c)
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	if len(rep.Uses) == 0 {
		fmt.Fprintf(stdout, "%s only uses SQLite's built-in collations, so there's nothing to convert.\n", clean(src))
		return nil
	}
	p, err := collate.MakePlan(c, rep, m)
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	printPlan(stdout, src, out, p)
	if err := printAffected(stdout, *queries, p); err != nil {
		return err
	}
	if p.Blocked() {
		fmt.Fprintln(stdout, "Nothing was written.")
		return errFailed
	}
	if !*write {
		fmt.Fprintf(stdout, "This was a dry run, so nothing was written. Run again with -write to create %s.\n", clean(out))
		return nil
	}
	if err := collate.Write(ctx, c, src, out, p, m); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Wrote %s. It holds exactly the same rows, and passed its integrity check. %s is unchanged.\n", clean(out), clean(src))
	return nil
}

func printPlan(w io.Writer, src, out string, p collate.Plan) {
	fmt.Fprintf(w, "%s → %s\n", clean(src), clean(out))
	var targets []string
	for _, c := range p.Changes {
		fmt.Fprintf(w, "  %s → %s\n", clean(c.String()), strings.ToUpper(c.To))
		if !slices.Contains(targets, c.To) {
			targets = append(targets, c.To)
		}
	}
	for _, name := range p.Unmapped {
		fmt.Fprintf(w, "  ! nothing replaces %s yet; add -to nocase, say, or -map %s=nocase\n", clean(name), clean(name))
	}
	for _, c := range p.Collisions {
		fmt.Fprintf(w, "  ! %s would hold %d rows on %s that are now different but would be equal, for example %s; please resolve them first\n",
			clean(c.Index), c.Rows, clean(c.Table), clean(c.Example))
	}
	for _, idx := range p.Unchecked {
		fmt.Fprintf(w, "  ! %s couldn't be checked for rows that would become equal, so Narcissus won't convert it\n", clean(idx))
	}
	for _, v := range p.Unsupported {
		fmt.Fprintf(w, "  ! the virtual table %s can't be recreated by Narcissus\n", clean(v))
	}
	for _, problem := range p.Problems {
		fmt.Fprintf(w, "  ! %s\n", clean(problem))
	}
	for _, note := range p.Notes {
		fmt.Fprintf(w, "  note: %s\n", clean(note))
	}
	if !p.Blocked() {
		fmt.Fprintln(w, "No rows would become equal in a UNIQUE index or primary key.")
	}
	for _, t := range targets {
		fmt.Fprintf(w, "After the change, %s %s. Sorting changes to match.\n", strings.ToUpper(t), collate.Meaning(t))
	}
}

// printAffected lists queries that read a column whose collation changes.
func printAffected(w io.Writer, path string, p collate.Plan) error {
	if path == "" {
		return nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	qs, err := check.ParseQueries(path, string(src))
	if err != nil {
		return err
	}
	cols := make(map[string]bool)
	for _, c := range p.Changes {
		if c.Column != "" && c.Column != "(expression)" {
			cols[sqltext.Fold(c.Column)] = true
		}
	}
	for _, q := range qs {
		toks, err := sqltext.Lex(q.SQL)
		if err != nil {
			continue
		}
		if slices.ContainsFunc(toks, func(t sqltext.Token) bool { return cols[sqltext.Fold(t.Ident())] }) {
			fmt.Fprintf(w, "  query %s:%d %suses a column whose collation changes\n", q.File, q.Line, label(q))
		}
	}
	return nil
}
