package index

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func mustParse(t *testing.T, src string) []Index {
	t.Helper()
	ixs, err := Parse("indexes.sql", src)
	if err != nil {
		t.Fatal(err)
	}
	return ixs
}

func TestParse(t *testing.T) {
	ixs := mustParse(t, `-- orders
CREATE INDEX orders_customer ON orders (customer_id, created_at DESC);

create unique index if not exists main."Users_Email"
  on [users](email) where deleted_at is null;
`)
	type got struct {
		Name, Table string
		Unique      bool
		Line        int
		SQL         string
	}
	var gots []got
	for _, ix := range ixs {
		gots = append(gots, got{ix.Name, ix.Table, ix.Unique, ix.Line, ix.SQL})
	}
	want := []got{
		{"orders_customer", "orders", false, 2, "CREATE INDEX orders_customer ON orders (customer_id, created_at DESC)"},
		{"Users_Email", "users", true, 4, `create unique index if not exists main."Users_Email" on [users](email) where deleted_at is null`},
	}
	if !reflect.DeepEqual(gots, want) {
		t.Errorf("Parse() =\n%+v\nwant\n%+v", gots, want)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct{ src, wantErr string }{
		{"CREATE TABLE t (a)", "indexes.sql:1: want only CREATE INDEX statements"},
		{"SELECT 1", "want only CREATE INDEX"},
		{"CREATE INDEX i ON t (a);\n\nCREATE INDEX I ON u (b)", "indexes.sql:3: index I already declared on line 1"},
		{"CREATE INDEX temp.i ON t (a)", `indexes in schema "temp" are not supported`},
		{"CREATE INDEX i t (a)", "want ON"},
		{"CREATE INDEX i ON t", "want a table name and column list"},
		{"CREATE INDEX IF EXISTS i ON t (a)", "want IF NOT EXISTS"},
		{"CREATE INDEX i ON t (a = 'x)", "indexes.sql:1: unterminated '"},
	}
	for _, tt := range tests {
		_, err := Parse("indexes.sql", tt.src)
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("Parse(%q) error = %v, want %q", tt.src, err, tt.wantErr)
		}
	}
}

func TestEqual(t *testing.T) {
	base := mustParse(t, `CREATE INDEX orders_x ON orders (status) WHERE status <> 'done'`)[0]
	tests := []struct {
		sql  string
		want bool
	}{
		{`CREATE INDEX orders_x ON "orders" (status)  WHERE status   <> 'done'`, true},
		{`create index main.ORDERS_X on [Orders](Status) where STATUS<>'done' -- note`, true},
		{`CREATE INDEX IF NOT EXISTS orders_x ON orders (status) WHERE status <> 'done'`, true},
		{`CREATE INDEX orders_x ON orders (status) WHERE status <> 'Done'`, false},
		{`CREATE UNIQUE INDEX orders_x ON orders (status) WHERE status <> 'done'`, false},
		{`CREATE INDEX orders_x ON orders (status DESC) WHERE status <> 'done'`, false},
		{`CREATE INDEX orders_x ON shipments (status) WHERE status <> 'done'`, false},
		{`CREATE INDEX 'orders_x' ON 'orders' (status) WHERE status <> 'done'`, true},
		{`CREATE INDEX orders_x ON orders ([status]) WHERE status <> 'done'`, true},
		{`CREATE INDEX orders_x ON orders ("status") WHERE status <> 'done'`, false},
	}
	for _, tt := range tests {
		ix, err := FromSQL(tt.sql)
		if err != nil {
			t.Fatal(err)
		}
		if got := base.Equal(ix); got != tt.want {
			t.Errorf("Equal(%q) = %v, want %v", tt.sql, got, tt.want)
		}
	}
}

func TestDiff(t *testing.T) {
	desired := mustParse(t, `
CREATE INDEX a_x ON a (x);
CREATE UNIQUE INDEX a_y ON a (y);
CREATE INDEX b_z ON b (z);
`)
	actual := []Index{}
	for _, sql := range []string{
		`CREATE INDEX a_x ON a (x)`,   // same
		`CREATE INDEX A_Y ON a (y)`,   // now unique
		`CREATE INDEX b_old ON b (q)`, // not desired
		`CREATE INDEX a_old ON a (q)`, // not desired
	} {
		ix, err := FromSQL(sql)
		if err != nil {
			t.Fatal(err)
		}
		actual = append(actual, ix)
	}
	names := func(p Plan) (changes, unmanaged []string) {
		for _, c := range p.Changes {
			changes = append(changes, c.Action.String()+" "+c.Index.Name)
		}
		for _, ix := range p.Unmanaged {
			unmanaged = append(unmanaged, ix.Name)
		}
		return
	}

	changes, unmanaged := names(Diff(desired, actual, PruneNone))
	if want := []string{"replace a_y", "create b_z"}; !reflect.DeepEqual(changes, want) {
		t.Errorf("changes = %q, want %q", changes, want)
	}
	if want := []string{"a_old", "b_old"}; !reflect.DeepEqual(unmanaged, want) {
		t.Errorf("unmanaged = %q, want %q", unmanaged, want)
	}

	changes, unmanaged = names(Diff(desired, actual, PruneAll))
	if want := []string{"drop a_old", "drop b_old", "replace a_y", "create b_z"}; !reflect.DeepEqual(changes, want) {
		t.Errorf("pruned changes = %q, want %q", changes, want)
	}
	if unmanaged != nil {
		t.Errorf("pruned unmanaged = %q, want none", unmanaged)
	}

	if p := Diff(desired, desired, PruneAll); !p.Empty() {
		t.Errorf("Diff(desired, desired) = %+v, want empty", p)
	}
}

func TestNonASCIINames(t *testing.T) {
	ixs := mustParse(t, `CREATE INDEX "É" ON t (a); CREATE INDEX "é" ON t (a);`)
	if len(ixs) != 2 {
		t.Fatalf("Parse() = %+v, want two indexes", ixs)
	}
	a := mustParse(t, `CREATE INDEX i ON t ("É")`)[0]
	b := mustParse(t, `CREATE INDEX i ON t ("é")`)[0]
	if a.Equal(b) {
		t.Error(`columns "É" and "é" compare equal; SQLite folds ASCII only`)
	}
}

func TestRaw(t *testing.T) {
	raw := Raw("odd", "t", "CREATE INDEX odd ON t (a) -- something Parse can't read")
	p := Diff(nil, []Index{raw}, PruneAll)
	if len(p.Changes) != 1 || p.Changes[0].Action != Drop || p.Changes[0].Index.Name != "odd" {
		t.Errorf("Diff() = %+v, want the raw index dropped", p)
	}
}

func TestParseRejectsHiddenStatements(t *testing.T) {
	_, err := Parse("indexes.sql", "CREATE INDEX i ON t (a) WHERE a > 0xE--'\n; DROP TABLE precious; --'\n;")
	if err == nil || !strings.Contains(err.Error(), "want only CREATE INDEX") {
		t.Errorf("Parse() = %v, want the DROP TABLE rejected", err)
	}
}

func TestPruneNonUnique(t *testing.T) {
	actual := mustParse(t, "CREATE UNIQUE INDEX u ON t (a); CREATE INDEX n ON t (b);")
	p := Diff(nil, actual, PruneNonUnique)
	if len(p.Changes) != 1 || p.Changes[0].Index.Name != "n" || len(p.Unmanaged) != 1 || p.Unmanaged[0].Name != "u" {
		t.Errorf("Diff(PruneNonUnique) = %+v, want n dropped and u kept", p)
	}
}

func TestShape(t *testing.T) {
	ok := []string{
		"CREATE INDEX i ON t (a, b DESC, c COLLATE NOCASE ASC)",
		"CREATE INDEX i ON t (lower(trim(email)))",
		"CREATE INDEX i ON t (a) WHERE deleted_at IS NULL AND n > -1",
		"CREATE INDEX i ON t (a) WHERE status IN ('open', 'held') OR NOT (k = 5)",
		"CREATE INDEX i ON t (a) WHERE n BETWEEN 1 AND 10 AND flag = 1 AND x NOTNULL",
		"CREATE INDEX i ON t (a) WHERE 0 < n",
	}
	refused := []string{
		"CREATE INDEX i ON t (customer || ':' || total)",
		"CREATE INDEX i ON t (CAST(a AS TEXT))",
		"CREATE INDEX i ON t ((a + b))",
		"CREATE INDEX i ON t (length(a))",
		"CREATE INDEX i ON t (\"lower\"(a))",
		"CREATE INDEX i ON t (data ->> '$.ref')",
		"CREATE INDEX i ON t (a) WHERE s = 1e15",
		"CREATE INDEX i ON t (a) WHERE n = 1.5",
		"CREATE INDEX i ON t (a) WHERE n = 1_000",
		"CREATE INDEX i ON t (a) WHERE s = r - 0",
		"CREATE INDEX i ON t (a) WHERE s = +r",
		"CREATE INDEX i ON t (a) WHERE a = b",
		"CREATE INDEX i ON t (a) WHERE a + 0 > 1",
		"CREATE INDEX i ON t (a) WHERE name = 'x' COLLATE NOCASE",
		"CREATE INDEX i ON t (a) WHERE n = 1234567890123456",
		"CREATE INDEX i ON t (a) WHERE flag IS TRUE",
		"CREATE INDEX i ON t (a) WHERE a = FALSE",
	}
	for _, sql := range append(ok, refused...) {
		ix, err := FromSQL(sql)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = ix.Shape()
		if want := slices.Contains(refused, sql); (err != nil) != want {
			t.Errorf("Shape(%q) = %v, want refused %v", sql, err, want)
		}
	}
}
