package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/kistdb/narcissus/internal/check"
	"github.com/kistdb/narcissus/internal/scratch"
)

func runCheck(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := newFlagSet("check", stderr)
	schema := flags.String("schema", "schema.sql", "the tables: a SQL `file`, database file or migrations folder")
	indexes := flags.String("indexes", "indexes.sql", "the `file` of desired indexes")
	queries := flags.String("queries", "queries.sql", "the `file` of queries to check")
	format := flags.String("format", "text", "output `format`: text, github or json")
	if err := parse(flags, args, 0); err != nil {
		return err
	}
	if *format != "text" && *format != "github" && *format != "json" {
		return usageError{err: fmt.Errorf("check: unknown format %q", *format)}
	}

	if _, err := readIndexes(*indexes); err != nil {
		return err
	}
	db, err := scratch.Build(ctx, *schema, *indexes)
	if err != nil {
		return err
	}
	defer db.Close()
	src, err := os.ReadFile(*queries)
	if err != nil {
		return err
	}
	qs, err := check.ParseQueries(*queries, string(src))
	if err != nil {
		return err
	}
	rep, err := check.Run(db, qs)
	if err != nil {
		return err
	}

	switch *format {
	case "json":
		err = writeJSON(stdout, rep, *schema, db.Ignored)
	case "github":
		writeGitHub(stdout, rep, *schema, db.Ignored)
	default:
		writeText(stdout, rep, *schema, db.Ignored)
	}
	if err != nil {
		return err
	}
	if rep.Failed() {
		return errFailed
	}
	return nil
}

func label(q check.Query) string {
	if q.Name == "" {
		return ""
	}
	return q.Name + ": "
}

func writeText(w io.Writer, rep *check.Report, schema string, ignored []string) {
	failed := 0
	for _, r := range rep.Results {
		q := r.Query
		if r.Failed() {
			failed++
		}
		if r.Err != nil {
			fmt.Fprintf(w, "%s:%d: %s%v\n", q.File, q.Line, label(q), r.Err)
		}
		for _, p := range r.Problems {
			fmt.Fprintf(w, "%s:%d: %s%s: %s\n", q.File, q.Line, label(q), p.Rule, p.Detail)
		}
		for _, s := range r.Suggest {
			fmt.Fprintf(w, "  suggest: %s\n", s)
		}
		if len(r.Problems) > 0 && len(r.Suggest) == 0 {
			fmt.Fprintf(w, "  no index would fix this; consider rewriting the query, or allow it with -- narcissus: allow %s\n", r.Problems[0].Rule)
		}
	}
	for _, ix := range rep.Unused {
		fmt.Fprintf(w, "%s:%d: warning: index %s is not used by any query\n", ix.File, ix.Line, ix.Name)
	}
	for _, name := range ignored {
		fmt.Fprintf(w, "%s: warning: index %s is not in the indexes file, so was left out\n", schema, name)
	}
	fmt.Fprintf(w, "%d %s checked, %d failed.\n", len(rep.Results), plural(len(rep.Results), "query", "queries"), failed)
}

func writeGitHub(w io.Writer, rep *check.Report, schema string, ignored []string) {
	for _, r := range rep.Results {
		if !r.Failed() {
			continue
		}
		q := r.Query
		var lines []string
		if r.Err != nil {
			lines = append(lines, r.Err.Error())
		}
		for _, p := range r.Problems {
			lines = append(lines, string(p.Rule)+": "+p.Detail)
		}
		for _, s := range r.Suggest {
			lines = append(lines, "suggest: "+s)
		}
		fmt.Fprintf(w, "::error file=%s,line=%d,title=%s::%s\n",
			ghProp(q.File), q.Line, ghProp("narcissus "+strings.TrimSuffix(label(q), ": ")), ghData(strings.Join(lines, "\n")))
	}
	for _, ix := range rep.Unused {
		fmt.Fprintf(w, "::warning file=%s,line=%d::%s\n", ghProp(ix.File), ix.Line, ghData("index "+ix.Name+" is not used by any query"))
	}
	for _, name := range ignored {
		fmt.Fprintf(w, "::warning file=%s::%s\n", ghProp(schema), ghData("index "+name+" is not in the indexes file, so was left out"))
	}
}

// ghData and ghProp escape GitHub Actions workflow command values.
func ghData(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

func ghProp(s string) string {
	return strings.NewReplacer(":", "%3A", ",", "%2C").Replace(ghData(s))
}

type jsonResult struct {
	Name     string        `json:"name,omitempty"`
	File     string        `json:"file"`
	Line     int           `json:"line"`
	Error    string        `json:"error,omitempty"`
	Problems []jsonProblem `json:"problems,omitempty"`
	Suggest  []string      `json:"suggest,omitempty"`
}

type jsonProblem struct {
	Rule   check.Rule `json:"rule"`
	Detail string     `json:"detail"`
}

type jsonIndex struct {
	Name string `json:"name"`
	Line int    `json:"line"`
}

func writeJSON(w io.Writer, rep *check.Report, schema string, ignored []string) error {
	out := struct {
		Failed  bool         `json:"failed"`
		Queries []jsonResult `json:"queries"`
		Unused  []jsonIndex  `json:"unused_indexes"`
		Ignored []string     `json:"ignored_schema_indexes"`
	}{Failed: rep.Failed(), Queries: []jsonResult{}, Unused: []jsonIndex{}, Ignored: ignored}
	if out.Ignored == nil {
		out.Ignored = []string{}
	}
	for _, r := range rep.Results {
		jr := jsonResult{Name: r.Query.Name, File: r.Query.File, Line: r.Query.Line, Suggest: r.Suggest}
		if r.Err != nil {
			jr.Error = r.Err.Error()
		}
		for _, p := range r.Problems {
			jr.Problems = append(jr.Problems, jsonProblem{p.Rule, p.Detail})
		}
		out.Queries = append(out.Queries, jr)
	}
	for _, ix := range rep.Unused {
		out.Unused = append(out.Unused, jsonIndex{ix.Name, ix.Line})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

var synopses = map[string]string{
	"check": "narcissus check [flags]",
	"plan":  "narcissus plan [flags] DB...",
	"apply": "narcissus apply [flags] DB...",
	"init":  "narcissus init [flags] DB",
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s\n\nFlags:\n", synopses[name])
		flags.PrintDefaults()
	}
	return flags
}

// parse parses args, wanting exactly nargs positional arguments, or at least one if nargs < 0.
func parse(flags *flag.FlagSet, args []string, nargs int) error {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err: err, printed: true}
	}
	for _, a := range flags.Args() {
		if strings.HasPrefix(a, "-") {
			return usageError{err: fmt.Errorf("%s: flags go before database names (found %s after them)", flags.Name(), a)}
		}
	}
	switch {
	case nargs < 0 && flags.NArg() == 0:
		return usageError{err: fmt.Errorf("%s: please give one or more databases", flags.Name())}
	case nargs == 1 && flags.NArg() != 1:
		return usageError{err: fmt.Errorf("%s: please give one database", flags.Name())}
	case nargs == 0 && flags.NArg() != 0:
		return usageError{err: fmt.Errorf("%s: doesn't take a database; use -schema", flags.Name())}
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// readIndexes reads the indexes file, pointing a new user to init if it's missing.
func readIndexes(path string) ([]byte, error) {
	src, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s doesn't exist yet; narcissus init DB writes one from a database, or use -indexes", path)
	}
	return src, err
}
