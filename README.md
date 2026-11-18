# Narcissus

**Index checks and index sync for SQLite.**

Narcissus catches slow SQLite queries in CI, before they ship. It reads each query's plan, fails the build when a query reads a whole table or sorts without an index, and tells you which index to add. Then it applies your indexes to one database or to thousands of them.

```
$ narcissus check
queries.sql:5: recent_orders: full-scan: SCAN orders
queries.sql:5: recent_orders: temp-btree: USE TEMP B-TREE FOR ORDER BY
  suggest: CREATE INDEX orders_customer_id_created_at ON orders (customer_id, created_at);
4 queries checked, 1 failed.
```

- **Plain SQL.** Your indexes and queries live in `.sql` files next to your code. No config format to learn.
- **No database needed in CI.** `check` builds a scratch database in memory from your schema, a database file or your migrations folder.
- **Made for many databases.** One database per tenant? `narcissus apply 'tenants/*.db'` brings them all in line, each in its own transaction.
- **One binary, no cgo.** SQLite is built in, so it runs anywhere Go does.

> **Status:** early and unreleased. Install from source for now; release binaries for Linux, macOS and Windows will follow.

> **Narcissus changes your databases.** It comes with no warranty: see [No warranty](#no-warranty). Keep backups, and run `plan` before `apply`.

## Install

With Go 1.27 or later:

```sh
go install github.com/kistdb/narcissus/cmd/narcissus@latest
```

## Quick start

Start from an existing database:

```sh
narcissus init app.db
```

That writes three files for you to commit:

| File | Holds |
|---|---|
| `schema.sql` | Your tables, views and triggers. |
| `indexes.sql` | Every index you want, as `CREATE INDEX` statements. This file owns your indexes. |
| `queries.sql` | The queries that matter. You fill this one in. |

Name each query in a comment:

```sql
-- name: recent_orders
SELECT * FROM orders WHERE customer_id = ? ORDER BY created_at DESC LIMIT 20;

-- name: export_all
-- narcissus: allow full-scan
SELECT * FROM orders;
```

Then:

```sh
narcissus check            # do the queries use indexes?
narcissus plan app.db      # what would change in the database
narcissus apply app.db     # make the changes
```

To try it without a database of your own, run `narcissus check` in [`example/`](./example).

## Commands

### check

```
narcissus check [-schema schema.sql] [-indexes indexes.sql] [-queries queries.sql] [-format text|github|json]
```

`check` builds an in-memory database from the schema and `indexes.sql`, asks SQLite for each query's plan, and fails if a plan:

- reads a table without an index (`full-scan`);
- sorts or groups in a temporary b-tree (`temp-btree`);
- builds a throwaway index on every run (`automatic-index`).

For each failing query it suggests up to three indexes that fix it. It also warns about non-unique indexes no query uses. To allow a rule for one query, put `-- narcissus: allow rule, ...` above it.

`-schema` takes a SQL file, a database file or a migrations folder. A database file gives the most realistic plans, because its planner statistics are copied too. See [how check works](docs/check.md).

### plan and apply

```
narcissus plan  [-indexes indexes.sql] [-prune] [-exit-code] [-jobs 4] [-busy-timeout 5s] DB...
narcissus apply [-indexes indexes.sql] [-prune] [-jobs 4] [-busy-timeout 5s] DB...
```

`plan` shows the index changes each database needs to match `indexes.sql`, and `apply` makes them. Give one database, several, or a quoted glob:

```
$ narcissus apply 'tenants/*.db'
tenants/acme.db
  + CREATE INDEX orders_customer_id_created_at ON orders (customer_id, created_at)
  ~ CREATE INDEX orders_status ON orders (status) WHERE deleted_at IS NULL
tenants/globex.db: error: database is locked
1000 databases: 998 changed, 1 in sync, 1 failed.
```

- **Matching:** indexes are matched by name. A changed definition is dropped and created again. Layout, comments, quoting and keyword case don't count as changes.
- **Safety:** a database already in sync is only read. One that needs changes is changed in one transaction, so a failure leaves it as it was. One failed database doesn't stop the others.
- **Pruning:** indexes not in `indexes.sql` are kept and listed, unless you pass `-prune`.
- **Locking:** building an index on a large table holds SQLite's write lock until it's done, so writers wait.

### init

```
narcissus init [-dir .] [-force] DB
```

`init` writes `schema.sql` and `indexes.sql` from a database, plus a starter `queries.sql`. It won't overwrite existing files without `-force`, and never replaces `queries.sql`. If your migrations create indexes, move them into `indexes.sql`: from then on, that file owns them.

### Exit status

| Status | Meaning |
|---|---|
| 0 | Success. |
| 1 | `check` found problems, a database failed, or another error. |
| 2 | A command-line mistake. |
| 3 | `plan -exit-code`: at least one database needs changes. |

## In CI

See [running Narcissus in CI](docs/ci.md). A GitHub Actions job that fails pull requests with slow queries:

```yaml
- uses: actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff # v5.6.0
  with:
    go-version: "1.27"
- run: go install github.com/kistdb/narcissus/cmd/narcissus@latest
- run: narcissus check -format github
```

## Limits

- **SQLite only.** Narcissus uses SQLite's own planner and parser on purpose.
- **Virtual tables:** FTS5 and R*Tree are built in. FTS3 and FTS4 aren't, so `check` can't load a schema that uses them.
- **Suggestions:** they use up to three plain columns. Expression and partial indexes are never suggested, though both are checked.
- **Flags first:** flags go before database names, as in `narcissus apply -prune 'tenants/*.db'`.

## No warranty

Narcissus is provided "as is", without warranty of any kind, express or implied, as set out in its MIT and Apache-2.0 licenses. You use it at your own risk. In no event are the authors or copyright holders liable for any claim, damages or other liability, including loss of or damage to data, arising from the software or its use.

`apply` drops and creates indexes in the databases you give it. Before you run it:

- back up the databases, and check that the backups restore;
- run `narcissus plan` on the same databases and read what it will change;
- try it on a copy first.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Changes are listed in [CHANGELOG.md](CHANGELOG.md).

## License

Licensed under either of

- Apache License, Version 2.0 ([LICENSE-APACHE](./LICENSE-APACHE))
- MIT license ([LICENSE-MIT](./LICENSE-MIT))

at your option.

Unless you explicitly state otherwise, any contribution intentionally submitted for inclusion in this work, as defined in the Apache-2.0 license, shall be dual licensed as above, without any additional terms or conditions.
