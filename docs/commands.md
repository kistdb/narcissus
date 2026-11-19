# Commands

Flags always go before file and database names. Add `-h` to see a command's flags, as in `narcissus apply -h`.

## check

```
narcissus check [-schema schema.sql] [-indexes indexes.sql] [-queries queries.sql] [-format text|github|json]
```

`check` builds a throwaway database in memory from your schema and `indexes.sql`, then asks SQLite how it would run each query. A query fails if its plan:

- reads a table without using an index (`full-scan`);
- sorts or groups its results in a temporary structure (`temp-btree`);
- builds a temporary index every time it runs (`automatic-index`).

For each failing query, `check` suggests up to three indexes. It also mentions indexes that no query uses, leaving out UNIQUE indexes and those that support a foreign key.

`-schema` can be a SQL file, a database file or a migrations folder. A copy of a real database gives the most realistic plans. [How check works](check.md) has the details, including the JSON format.

## plan and apply

```
narcissus plan  [flags] DB...
narcissus apply [flags] DB...
```

`plan` shows the index changes each database needs to match `indexes.sql`; `apply` makes them. `DB` is a database file, several files, or a glob in quotes such as `'tenants/*.db'`. `*`, `?` and `[...]` match within one folder level; for nested folders, write each level, as in `'tenants/*/*.db'`.

```
$ narcissus apply 'tenants/*.db'
tenants/acme.db
  ~ CREATE INDEX orders_status ON orders (status) WHERE deleted_at IS NULL
  + CREATE INDEX orders_customer_id_created_at ON orders (customer_id, created_at)
tenants/globex.db: error: database is locked
1000 databases: 998 changed, 1 in sync, 1 failed.
```

`+` creates an index, `~` replaces one whose definition has changed, `-` drops one, and `!` is a note or a reason something can't be done.

**What to expect:**

- **Every change is checked first.** If any would be refused, nothing in that database is changed, and the reasons are listed, so please fix or remove a refused line before relying on the others. `plan` runs the same checks, so it tells you in advance.
- **One transaction per database.** A database that needs changes is changed in a single transaction, so if anything goes wrong it's left exactly as it was. One database failing doesn't stop the others.
- **Indexes are matched by name.** Layout, comments and the case of keywords and names don't count as changes. Double-quoted names are compared exactly as written, so `("email")` and `(email)` count as different, and the index is rebuilt once. A double-quoted name must be a real column: if it no longer exists, say after a column is renamed, Narcissus stops rather than treating it as text.
- **Indexes not in `indexes.sql`** are kept and listed. `-prune` drops them, except UNIQUE ones, which enforce a rule on your data: dropping or replacing those also needs `-prune-unique`. Narcissus won't prune when `indexes.sql` is empty.
- **Some indexes are best created by your app.** Narcissus declines indexes that a different SQLite might compute differently; see [Keeping your databases safe](safety.md#which-indexes-narcissus-builds).
- **Building an index** holds SQLite's write lock until it finishes. Meanwhile your app's writes wait, and fail with "database is locked" if their own busy timeout runs out first; in rollback-journal mode, reads wait too. Large changes are best made at a quiet time.
- **Ctrl-C is safe,** as are closing the terminal and piping the output into something that stops early. A database part-way through is rolled back; the ones not yet started are counted at the end.

| Flag | Default | What it does |
|---|---|---|
| `-indexes` | `indexes.sql` | The file of desired indexes. |
| `-prune` | off | Drop indexes that aren't in `indexes.sql`, except UNIQUE ones. |
| `-prune-unique` | off | With `-prune`, drop UNIQUE ones too. |
| `-allow-collations-and-functions` | off | Also build `NOCASE`, `lower()`, `upper()` and `trim()`; see [which indexes Narcissus builds](safety.md#which-indexes-narcissus-builds). |
| `-exit-code` | off | `plan` only: exit with status 3 if any database needs changes. |
| `-jobs` | 4 | How many databases to work on at once. |
| `-busy-timeout` | 5s | How long to wait for a database another program has locked. |

## init

```
narcissus init [-dir .] [-force] DB
```

`init` writes `schema.sql` and `indexes.sql` from a database, plus a starter `queries.sql`. It won't overwrite files that are already there unless you add `-force`, and it never replaces `queries.sql`. It also lists any existing indexes Narcissus wouldn't build itself.

## collations and convert

```
narcissus collations [-format text|json] DB...
narcissus convert -to COLLATION [-map NAME=COLLATION,...] [-queries queries.sql] [-write] DB NEW
```

A database whose app used its own collation, such as Android's `LOCALIZED`, can't be sorted or compared by other SQLite programs. `collations` lists those collations, and `convert` writes a copy that uses SQLite's built-in ones. `-to` (`binary`, `nocase` or `rtrim`) replaces every other collation, and `-map` names them one by one; there's no default, so please give at least one. It's a dry run unless you add `-write`, and it never changes the original. See [Collations other programs don't have](collations.md).

## version

```
narcissus version
```

Shows the Narcissus version and the SQLite built into it.

## Exit status

| Status | Meaning |
|---|---|
| 0 | All went well. |
| 1 | `check` found a problem, `collations` found other collations, a database couldn't be changed or converted, or something else went wrong. |
| 2 | Something on the command line needs a look. |
| 3 | `plan -exit-code`: at least one database needs changes. |
