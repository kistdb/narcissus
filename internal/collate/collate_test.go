package collate

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3"

	"github.com/kistdb/narcissus/internal/sqlite"
)

// appDB makes a database as an app with its own LOCALIZED collation would.
// It's stricter than NOCASE, so rows can collide once converted.
func appDB(t *testing.T, sql string) string {
	return appDBWith(t, func(a, b []byte) int { return bytes.Compare(b, a) }, sql)
}

// caseless is a LOCALIZED that ignores all case, looser than NOCASE.
func caseless(a, b []byte) int {
	return strings.Compare(strings.ToLower(string(a)), strings.ToLower(string(b)))
}

func appDBWith(t *testing.T, localized func(a, b []byte) int, sql string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.db")
	c, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.CreateCollation("LOCALIZED", localized); err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(sql); err != nil {
		t.Fatal(err)
	}
	return path
}

func open(t *testing.T, path string) *sqlite3.Conn {
	t.Helper()
	c, err := sqlite.Open(t.Context(), path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func query(t *testing.T, c *sqlite3.Conn, sql string) []string {
	t.Helper()
	var rows []string
	err := sqlite.Query(c, sql, func(st *sqlite3.Stmt) error {
		var cols []string
		for i := range st.ColumnCount() {
			cols = append(cols, st.ColumnText(i))
		}
		rows = append(rows, strings.Join(cols, "|"))
		return nil
	})
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return rows
}

const everything = `
PRAGMA journal_mode = WAL; PRAGMA user_version = 42; PRAGMA application_id = 1234;
CREATE TABLE people (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT COLLATE LOCALIZED UNIQUE, city TEXT, born INT,
	shout TEXT AS (upper(name)) VIRTUAL, kept TEXT AS (city || '!') STORED, boss INT REFERENCES people (id));
CREATE TABLE tags (tag TEXT COLLATE LOCALIZED PRIMARY KEY, n INT) WITHOUT ROWID;
CREATE TABLE notes (body TEXT, who INT);
CREATE INDEX people_city ON people (city COLLATE LOCALIZED) WHERE born > 1900;
CREATE INDEX people_born ON people (born DESC);
CREATE VIEW sorted AS SELECT name FROM people ORDER BY name COLLATE LOCALIZED;
CREATE TRIGGER people_log AFTER INSERT ON people BEGIN INSERT INTO notes VALUES ('added ' || new.name, new.id); END;
CREATE VIRTUAL TABLE docs USING fts5(body);
CREATE VIRTUAL TABLE boxes USING rtree(id, x0, x1);
INSERT INTO people (name, city, born, boss) VALUES ('Émile', 'Paris', 1950, NULL), ('zoe', 'oslo', 1980, 1), ('Zoë', 'Oslo', 1990, 1);
DELETE FROM people WHERE id = 3; INSERT INTO people (name, city, born) VALUES ('Zoë', 'Oslo', 1990);
INSERT INTO tags VALUES ('Red', 1), ('blue', 2);
INSERT INTO notes (rowid, body, who) VALUES (100, 'hello', 1);
INSERT INTO docs (rowid, body) VALUES (7, 'searchable text');
INSERT INTO boxes VALUES (1, 0, 10);
`

func TestFind(t *testing.T) {
	c := open(t, appDB(t, everything))
	r, err := Find(c)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range r.Uses {
		got = append(got, u.String())
	}
	for _, want := range []string{"table people, column name: LOCALIZED", "table tags, column tag: LOCALIZED",
		"view sorted: LOCALIZED", "index people_city, key city: LOCALIZED"} {
		if !slices.Contains(got, want) {
			t.Errorf("Find() = %q, missing %q", got, want)
		}
	}
	if !r.Found() {
		t.Error("Found() = false")
	}
}

func TestConvert(t *testing.T) {
	src := appDB(t, everything)
	c := open(t, src)
	r, err := Begin(c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseMapping("nocase", "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := MakePlan(c, r, m)
	if err != nil {
		t.Fatal(err)
	}
	if p.Blocked() {
		t.Fatalf("plan blocked: %+v", p)
	}
	out := filepath.Join(t.TempDir(), "out.db")
	if err := Write(t.Context(), c, src, out, p, m); err != nil {
		t.Fatal(err)
	}
	o := open(t, out)
	checks := map[string]string{
		`PRAGMA integrity_check`: "ok",
		`PRAGMA user_version`:    "42",
		`PRAGMA application_id`:  "1234",
		`PRAGMA journal_mode`:    "wal",
		`SELECT group_concat(id || name, ',') FROM (SELECT * FROM people ORDER BY id)`:           "1Émile,2zoe,4Zoë",
		`SELECT seq FROM sqlite_sequence WHERE name = 'people'`:                                  "4",
		`SELECT shout || kept FROM people WHERE id = 2`:                                          "ZOEoslo!",
		`SELECT group_concat(rowid || body) FROM (SELECT rowid, body FROM notes ORDER BY rowid)`: "1added Émile,2added zoe,3added Zoë,4added Zoë,100hello",
		`SELECT rowid FROM docs WHERE docs MATCH 'searchable'`:                                   "7",
		`SELECT id FROM boxes WHERE x0 >= 0`:                                                     "1",
		`SELECT count(*) FROM sqlite_schema WHERE sql LIKE '%LOCALIZED%'`:                        "0",
		`SELECT count(*) FROM tags WHERE tag = 'RED'`:                                            "1",
		`SELECT count(*) FROM people WHERE name = 'ZOE'`:                                         "1",
	}
	for q, want := range checks {
		if got := strings.Join(query(t, o, q), ","); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
	if _, err := os.Stat(out + ".narcissus-tmp"); err == nil {
		t.Error("temporary file left behind")
	}
	if err := Write(t.Context(), c, src, out, p, m); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second Write() = %v, want a refusal", err)
	}
}

func TestPlanBlocks(t *testing.T) {
	tests := []struct {
		name, sql, to, pairs, want string
	}{
		{"unmapped", `CREATE TABLE t (a TEXT COLLATE LOCALIZED)`, "", "", "LOCALIZED"},
		{"collision", `CREATE TABLE t (a TEXT COLLATE LOCALIZED UNIQUE); INSERT INTO t VALUES ('Zoe'), ('zoë'), ('ZOE'), ('x');`,
			"", "LOCALIZED=nocase", "collision"},
		{"partial unique collision", `CREATE TABLE t (a TEXT COLLATE LOCALIZED, live INT);
			CREATE UNIQUE INDEX t_live ON t (a) WHERE live = 1; INSERT INTO t VALUES ('Ab', 1), ('aB', 1), ('ab', 0);`,
			"nocase", "", "collision"},
		{"expression unique", `CREATE TABLE t (a TEXT); CREATE UNIQUE INDEX t_e ON t (a || '' COLLATE LOCALIZED);`, "nocase", "", "unchecked"},
		{"generated column compares", `CREATE TABLE t (a TEXT COLLATE LOCALIZED, g AS (a < 'n') STORED);`, "nocase", "", "problem"},
		{"check compares", `CREATE TABLE t (a TEXT COLLATE LOCALIZED CHECK (a <> 'admin'));`, "nocase", "", "problem"},
		{"partial unique where", `CREATE TABLE t (email TEXT, kind TEXT COLLATE LOCALIZED);
			CREATE UNIQUE INDEX t_one ON t (email) WHERE kind = 'primary';`, "binary", "", "problem"},
		{"partial unique explicit collate", `CREATE TABLE t (email TEXT, kind TEXT);
			CREATE UNIQUE INDEX t_one ON t (email) WHERE kind = 'primary' COLLATE LOCALIZED;`, "nocase", "", "problem"},
		{"rowid hidden", `CREATE TABLE t (rowid, _rowid_, oid, a TEXT COLLATE LOCALIZED);`, "nocase", "", "problem"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := appDB(t, tt.sql)
			c := open(t, path)
			r, err := Begin(c)
			if err != nil {
				t.Fatal(err)
			}
			m, err := ParseMapping(tt.to, tt.pairs)
			if err != nil {
				t.Fatal(err)
			}
			p, err := MakePlan(c, r, m)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{"LOCALIZED": len(p.Unmapped) > 0, "collision": len(p.Collisions) > 0, "unchecked": len(p.Unchecked) > 0, "problem": len(p.Problems) > 0}
			if !got[tt.want] || !p.Blocked() {
				t.Errorf("plan = %+v, want %s", p, tt.want)
			}
			if err := Write(t.Context(), c, path, filepath.Join(t.TempDir(), "out.db"), p, m); err == nil {
				t.Error("Write() went ahead with a blocked plan")
			}
		})
	}
}

func TestNocaseOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	c, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// An app whose NOCASE also folds non-ASCII letters.
	err = c.CreateCollation("NOCASE", func(a, b []byte) int { return strings.Compare(strings.ToLower(string(a)), strings.ToLower(string(b))) })
	if err == nil {
		err = c.Exec(`CREATE TABLE t (a TEXT); CREATE INDEX t_a ON t (a COLLATE NOCASE);
			INSERT INTO t VALUES ('É'), ('f'), ('é'), ('E'), ('Z'), ('à');`)
	}
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	r, err := Find(open(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Overrides) == 0 || r.Overrides[0].Index != "t_a" {
		t.Errorf("Overrides = %+v, want t_a", r.Overrides)
	}
}

func TestParseMapping(t *testing.T) {
	m, err := ParseMapping("Binary", "LOCALIZED=nocase, unicode=RTRIM")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"localized": "nocase", "UNICODE": "rtrim", "other": "binary", "NOCASE": "nocase"} {
		if got, _ := m.Target(name); got != want {
			t.Errorf("Target(%s) = %s, want %s", name, got, want)
		}
	}
	for _, bad := range [][2]string{{"icu", ""}, {"", "X"}, {"", "X=icu"}, {"", "=nocase"}} {
		if _, err := ParseMapping(bad[0], bad[1]); err == nil {
			t.Errorf("ParseMapping(%q, %q) succeeded", bad[0], bad[1])
		}
	}
}

// plan opens path as convert does and plans a conversion to target.
func plan(t *testing.T, path, target string) (*sqlite3.Conn, Plan, Mapping) {
	t.Helper()
	c := open(t, path)
	r, err := Begin(c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseMapping(target, "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := MakePlan(c, r, m)
	if err != nil {
		t.Fatal(err)
	}
	return c, p, m
}

func TestForeignKeyOrphans(t *testing.T) {
	path := appDBWith(t, caseless, `CREATE TABLE parent (name TEXT COLLATE LOCALIZED PRIMARY KEY);
		CREATE TABLE child (pname REFERENCES parent (name));
		INSERT INTO parent VALUES ('Zoe'), ('adam'); INSERT INTO child VALUES ('zoe'), ('ADAM'), ('adam'), (NULL);`)
	_, p, _ := plan(t, path, "binary")
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "2 rows of child") {
		t.Errorf("Problems = %q, want 2 rows of child without a parent", p.Problems)
	}
}

func TestRewrite(t *testing.T) {
	m := Mapping{Default: "nocase"}
	for in, want := range map[string]string{
		`CREATE TABLE t(a COLLATE"LOCALIZED", b TEXT COLLATE[LOCALIZED], c)`:  `CREATE TABLE t(a COLLATE NOCASE, b TEXT COLLATE NOCASE, c)`,
		`CREATE VIEW v AS SELECT a FROM t ORDER BY a COLLATE "LOCALIZED"DESC`: `CREATE VIEW v AS SELECT a FROM t ORDER BY a COLLATE NOCASE DESC`,
		`CREATE INDEX i ON t(a COLLATE LOCALIZED)`:                            `CREATE INDEX i ON t(a COLLATE NOCASE)`,
		`CREATE TABLE t(a COLLATE nocase /* COLLATE LOCALIZED */)`:            `CREATE TABLE t(a COLLATE nocase /* COLLATE LOCALIZED */)`,
	} {
		if got, err := rewrite(in, m); err != nil || got != want {
			t.Errorf("rewrite(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestConvertExact(t *testing.T) {
	// Text that isn't valid UTF-16, an external content table named like a shadow table, and a private file.
	path := appDB(t, `PRAGMA encoding = 'UTF-16le';
		CREATE TABLE t (a TEXT COLLATE LOCALIZED, b TEXT UNIQUE ON CONFLICT REPLACE);
		INSERT INTO t VALUES (CAST(x'3DD8' AS TEXT), 'x'), (CAST(x'00DC4100' AS TEXT), 'y');
		CREATE VIRTUAL TABLE docs USING fts5(body, content='docs_content', content_rowid='id');
		CREATE TABLE docs_content (id INTEGER PRIMARY KEY, body);
		INSERT INTO docs_content VALUES (1, 'hello world'); INSERT INTO docs (rowid, body) VALUES (1, 'hello world');`)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	c, p, m := plan(t, path, "nocase")
	if p.Blocked() {
		t.Fatalf("plan blocked: %+v", p)
	}
	out := filepath.Join(t.TempDir(), "out.db")
	if err := Write(t.Context(), c, path, out, p, m); err != nil {
		t.Fatal(err)
	}
	o := open(t, out)
	for q, want := range map[string]string{
		`SELECT group_concat(hex(a), ',') FROM (SELECT a FROM t ORDER BY rowid)`: "3DD8,00DC4100",
		`SELECT rowid FROM docs WHERE docs MATCH 'hello'`:                        "1",
	} {
		if got := strings.Join(query(t, o, q), ","); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("out's mode = %v, %v; want 0600", info.Mode(), err)
	}
}

func TestVerifyCatchesLostRows(t *testing.T) {
	path := appDB(t, `CREATE TABLE t (a TEXT COLLATE LOCALIZED); INSERT INTO t VALUES ('a'), ('b');`)
	c, p, m := plan(t, path, "nocase")
	dst, err := sqlite.OpenMemory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	tables, err := copyContent(c, dst, m)
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(c, dst, p, tables); err != nil {
		t.Fatalf("verify() of a faithful copy = %v", err)
	}
	if err := dst.Exec(`UPDATE t SET a = 'B' WHERE a = 'b'`); err != nil {
		t.Fatal(err)
	}
	if err := verify(c, dst, p, tables); err == nil || !strings.Contains(err.Error(), "exactly") {
		t.Errorf("verify() of a changed copy = %v, want a refusal", err)
	}
}
