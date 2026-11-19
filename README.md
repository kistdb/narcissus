# Narcissus

**Index checks and index sync for SQLite.**

Narcissus helps you catch slow SQLite queries before they ship. It reads the plan SQLite makes for each of your queries, lets you know when one would read a whole table or sort without an index, and suggests the index that would help. When you're happy, it applies your indexes to one database or to thousands.

```
$ narcissus check
queries.sql:5: recent_orders: full-scan: SCAN orders
queries.sql:5: recent_orders: temp-btree: USE TEMP B-TREE FOR ORDER BY
  suggest: CREATE INDEX orders_customer_id_created_at ON orders (customer_id, created_at);
indexes.sql:2: warning: index orders_total is not used by any query
4 queries checked, 1 failed.
```

- **Plain SQL.** Your indexes and queries live in `.sql` files beside your code. There's no new format to learn.
- **No database needed in CI.** `check` builds a throwaway copy of your schema in memory, from a SQL file, a database file or your migrations.
- **Made for many databases.** If you keep one database per customer, `narcissus apply 'tenants/*.db'` brings them all in line, each in its own transaction.
- **One small binary.** SQLite is built in, so there's nothing else to install.

> **Before you start:** `apply` is the only command that changes your databases, and it only ever creates and drops indexes, never your tables or rows. Even so, please keep backups and run `plan` first. Narcissus comes with no warranty: see [No warranty](#no-warranty).

**Status:** early and not yet released. It's tested on macOS and Linux; Windows builds, and will be tested automatically once the repository is public. Ready-made downloads for Linux, macOS and Windows will follow.

## Install

You'll need Go 1.27 or later:

```sh
go install github.com/kistdb/narcissus/cmd/narcissus@latest
narcissus version
```

Go puts the binary in `$(go env GOPATH)/bin`. If `narcissus version` isn't found, add that folder to your `PATH`.

## Getting started

1. **Write the starting files from a database you already have:**

   ```sh
   narcissus init app.db
   ```

   This writes three files for you to commit:

   | File | Holds |
   |---|---|
   | `schema.sql` | Your tables, views and triggers. |
   | `indexes.sql` | Every index you want, as `CREATE INDEX` statements. From now on, this file is where your indexes live. |
   | `queries.sql` | The queries that matter to you. This one starts empty for you to fill in. |

2. **Add your important queries** to `queries.sql`, each with a name in a comment above it:

   ```sql
   -- name: recent_orders
   SELECT * FROM orders WHERE customer_id = ? ORDER BY created_at DESC LIMIT 20;
   ```

3. **Check them:**

   ```sh
   narcissus check
   ```

4. **Act on what it finds.** See [When check finds a problem](#when-check-finds-a-problem).

5. **See what would change in your database, then apply it:**

   ```sh
   narcissus plan app.db
   narcissus apply app.db
   ```

To try Narcissus without a database of your own, run `narcissus check` in the [`example/`](./example) folder. It fails one query on purpose, so you can see what a problem looks like.

## When check finds a problem

For each query that would be slow, you have three choices:

- **Add the suggested index.** Copy the `suggest:` line into `indexes.sql` and run `check` again. When you're ready, `plan` and `apply` will add it to your databases.
- **Rewrite the query** so it can use an index you already have.
- **Accept it**, if reading the whole table is what you want (an export, say). Put this above the query:

  ```sql
  -- name: export_all
  -- narcissus: allow full-scan
  SELECT * FROM orders;
  ```

Sometimes no single index would help, for example when a query filters on a calculation. Narcissus will say so, and you can rewrite the query or allow the rule.

## Commands

### check

```
narcissus check [-schema schema.sql] [-indexes indexes.sql] [-queries queries.sql] [-format text|github|json]
```

`check` builds a throwaway database in memory from your schema and `indexes.sql`, then asks SQLite how it would run each query. A query fails if its plan:

- reads a table without using an index (`full-scan`);
- sorts or groups its results in a temporary structure (`temp-btree`);
- builds a temporary index every time it runs (`automatic-index`).

For each failing query, `check` suggests up to three indexes. It also mentions indexes that no query uses (it leaves out UNIQUE indexes and those that support a foreign key). If you'd like to keep one anyway, put `-- narcissus: allow unused` above it in `indexes.sql`.

`-schema` can be a SQL file, a database file or a migrations folder. A copy of a real database gives the most realistic plans. See [How check works](docs/check.md) for the details, including the JSON format.

### plan and apply

```
narcissus plan  [flags] DB...
narcissus apply [flags] DB...
```

`plan` shows the index changes each database needs to match `indexes.sql`; `apply` makes them. `DB` is a database file, several files, or a glob in quotes such as `'tenants/*.db'`. Flags go before the database names.

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

- **Before anything changes,** every change is checked. If any would be refused, nothing in that database is changed, and the reasons are listed. `plan` runs the same checks, so it tells you in advance.
- **One transaction per database.** A database that's already in sync is only read. One that needs changes is changed in a single transaction, so if anything goes wrong it's left exactly as it was. One database failing doesn't stop the others.
- **Indexes are matched by name.** Layout, comments, keyword case and the case of names don't count as changes. Double-quoted names are compared exactly as written, so `("email")` and `(email)` count as different, and the index is rebuilt once. A double-quoted name must be a real column: if it no longer exists, say after a column is renamed, Narcissus stops rather than treating it as text.
- **Indexes not in `indexes.sql`** are kept and listed. `-prune` drops them, except UNIQUE ones, which enforce a rule on your data: dropping or replacing those also needs `-prune-unique`. Narcissus won't prune when `indexes.sql` is empty.
- **Some indexes are best created by your app.** Narcissus declines indexes that a different SQLite might compute differently; see [Which indexes Narcissus builds](#which-indexes-narcissus-builds).
- **Building an index** holds SQLite's write lock until it finishes. Meanwhile your app's writes wait, and fail with "database is locked" if their own busy timeout runs out first; in rollback-journal mode, reads wait too. Large changes are best made at a quiet time.
- **Ctrl-C is safe.** A database part-way through is rolled back; the ones not yet started are counted at the end.

| Flag | Default | What it does |
|---|---|---|
| `-indexes` | `indexes.sql` | The file of desired indexes. |
| `-prune` | off | Drop indexes that aren't in `indexes.sql`, except UNIQUE ones. |
| `-prune-unique` | off | With `-prune`, drop UNIQUE ones too. |
| `-allow-collations-and-functions` | off | Also build `NOCASE`, `RTRIM`, `lower()`, `upper()` and `trim()`; see [Which indexes Narcissus builds](#which-indexes-narcissus-builds). |
| `-exit-code` | off | `plan` only: exit with status 3 if any database needs changes. |
| `-jobs` | 4 | How many databases to work on at once. |
| `-busy-timeout` | 5s | How long to wait for a database another program has locked. |

### init

```
narcissus init [-dir .] [-force] DB
```

`init` writes `schema.sql` and `indexes.sql` from a database, plus a starter `queries.sql`. It won't overwrite files that are already there unless you add `-force`, and it never replaces `queries.sql`.

### Exit status

| Status | Meaning |
|---|---|
| 0 | All went well. |
| 1 | `check` found a problem, a database couldn't be changed, or something else went wrong. |
| 2 | Something on the command line needs a look. |
| 3 | `plan -exit-code`: at least one database needs changes. |

## Which indexes Narcissus builds

Narcissus builds indexes with its own copy of SQLite (run `narcissus version` to see which), and your app may use a different version or build. An index is only correct if both compute its keys the same way. So Narcissus builds only indexes that every SQLite computes identically:

- **Columns, in any order, ascending or descending.** For example `(customer_id, created_at DESC)`.
- **A partial index's WHERE** that compares columns with whole numbers, text or `NULL`, using `=`, `<`, `>`, `IS NULL`, `IN`, `BETWEEN`, `AND`, `OR` and `NOT`. Text is only compared with text columns. For example `WHERE deleted_at IS NULL AND status IN ('open', 'held')`.
- **With `-allow-collations-and-functions`,** also the `NOCASE` and `RTRIM` collations, and `lower()`, `upper()` and `trim()` of text columns. An app can replace these with its own versions, which is why they need the flag; if your app uses SQLite's standard ones, as most do, the flag is safe. For example `(lower(email))` or `(email COLLATE NOCASE)`.

A few data-dependent cases are declined too: a whole number compared with a numeric column that holds text in some rows, a column whose default is an unusual number (such as `1e-300`) that older rows don't store, and text comparisons or `lower()`, `upper()` and `trim()` in a UTF-16 database. Write `1` and `0` rather than `TRUE` and `FALSE`, which older SQLite doesn't know.

Anything else is declined, with a note saying why: other functions, JSON, arithmetic, casts, decimal numbers in a WHERE, and virtual generated columns. Different SQLite versions can compute those differently, sometimes only in the last digit of a decimal number. That's enough for an app to miss rows or reject valid ones. Please create those indexes from your app, so they're built with its own SQLite. `check` still checks queries against them.

## Using it with migrations

You don't need to change your old migrations.

1. Run `narcissus init` on a database that's had all your migrations applied, and commit `indexes.sql`.
2. Point `check` at your migrations folder with `-schema db/migrations`. Narcissus reads plain `.sql` files, golang-migrate, dbmate, goose, sql-migrate and Prisma. For anything else, point it at a migrated database file.
3. Add new indexes to `indexes.sql` only, not to new migrations.
4. Run `narcissus apply` straight after your migrations. If a migration rebuilds a table, which drops its indexes, `apply` puts them back.

## Undoing a change

Indexes hold no data of their own, so creating, dropping or rebuilding one never loses rows.

- **To remove an index,** delete its line from `indexes.sql`, then run `plan -prune` and `apply -prune`. These drop every index that isn't in the file, so do read the plan first. You can also `DROP INDEX` it yourself.
- **To restore an index you changed,** put the old line back and run `apply`.

While a UNIQUE index is gone, duplicate rows can slip in, so please take extra care with those.

## In CI

There's a short guide to [running Narcissus in CI](docs/ci.md). For GitHub Actions:

```yaml
- uses: actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff # v5.6.0
  with:
    go-version: "1.27.x"
- run: go install github.com/kistdb/narcissus/cmd/narcissus@latest
- run: narcissus check -format github
```

## Good to know

- **SQLite only.** Narcissus relies on SQLite's own planner and parser, on purpose.
- **Platforms.** Linux, macOS and Windows. On BSD and illumos, `apply` declines to change databases for now, because file locking there may not match other SQLite programs; we're looking into support for them.
- **Database files.** Narcissus opens only files that start with the SQLite header, and never creates a database. SQLite keeps its journal beside the database under the database's name, so a file with two names (hard links) would have two journals. That isn't safe, so Narcissus declines it. Symlinks are fine, and a file reached by several paths is handled once.
- **Side files.** Like any SQLite program, `plan` and `apply` may leave `-wal` and `-shm` files beside a database in WAL mode. Please run Narcissus as the user that owns your databases, not as root, so those files belong to the right user.
- **Globs.** `*`, `?` and `[...]` work within one folder level; for nested folders, write each level, as in `'tenants/*/*.db'`.
- **Virtual tables.** FTS5 and R*Tree are built in. FTS3 and FTS4 aren't, so `check` can't load a schema that uses them.

## No warranty

Narcissus is provided "as is", without warranty of any kind, express or implied, as set out in its MIT and Apache-2.0 licences. You use it at your own risk. In no event are the authors or copyright holders liable for any claim, damages or other liability, including loss of or damage to data, arising from the software or its use.

`apply` drops and creates indexes in the databases you give it. Before you run it:

- back up the databases, and check that the backups restore;
- run `narcissus plan` on the same databases and read what it will change;
- try it on a copy first.

## Contributing

Contributions are very welcome; see [CONTRIBUTING.md](CONTRIBUTING.md). Changes are listed in [CHANGELOG.md](CHANGELOG.md).

## Licence

Licensed under either of

- Apache License, Version 2.0 ([LICENSE-APACHE](./LICENSE-APACHE))
- MIT licence ([LICENSE-MIT](./LICENSE-MIT))

at your option.

Unless you explicitly state otherwise, any contribution intentionally submitted for inclusion in this work, as defined in the Apache-2.0 licence, shall be dual licensed as above, without any additional terms or conditions.
