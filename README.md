# Narcissus

[![tests](https://github.com/kistdb/narcissus/actions/workflows/test.yml/badge.svg)](https://github.com/kistdb/narcissus/actions/workflows/test.yml)
[![release](https://img.shields.io/github/v/release/kistdb/narcissus)](https://github.com/kistdb/narcissus/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/kistdb/narcissus)](go.mod)
[![licence](https://img.shields.io/badge/licence-MIT%20OR%20Apache--2.0-blue)](#licence)

**Index checks and index sync for SQLite.**

Narcissus helps you catch slow SQLite queries before they ship. It reads the plan SQLite makes for each query, tells you when one would read a whole table or sort without an index, and suggests the index that would help. When you're happy, it applies your indexes to one database or to thousands.

```
$ narcissus check
queries.sql:5: recent_orders: full-scan: SCAN orders
queries.sql:5: recent_orders: temp-btree: USE TEMP B-TREE FOR ORDER BY
  suggest: CREATE INDEX orders_customer_id_created_at ON orders (customer_id, created_at);
indexes.sql:2: warning: index orders_total is not used by any query
4 queries checked, 1 failed.
```

- **Plain SQL.** Your indexes and queries live in `.sql` files beside your code.
- **No database needed in CI.** `check` works from your schema, a database file or your migrations.
- **Made for many databases.** `narcissus apply 'tenants/*.db'` brings them all in line, each in its own transaction.
- **One small binary,** with SQLite built in.

**Status:** early (version 0.1), so commands and output may still change. Tested on Linux, macOS and Windows.

## Install

Download `narcissus` for Linux, macOS or Windows from the [releases page](https://github.com/kistdb/narcissus/releases/latest) and put it on your `PATH` (on macOS, see [the note on first run](docs/getting-started.md#on-macos)). Or, with Go 1.27 or later:

```sh
go install github.com/kistdb/narcissus/cmd/narcissus@latest
```

## Quick start

```sh
narcissus init app.db      # write schema.sql, indexes.sql and queries.sql from your database
                           # then add the queries that matter to queries.sql
narcissus check            # are they using indexes?
narcissus plan app.db      # what would change in the database
narcissus apply app.db     # make those changes
```

`apply` only ever creates and drops indexes, never your tables or rows. Even so, please keep backups and read the `plan` first.

To try it without a database of your own, run `narcissus check` in the [`example/`](./example) folder.

## Documentation

- [Getting started](docs/getting-started.md): installing, your first check, and what to do when it finds a problem.
- [Commands](docs/commands.md): every command, flag and exit status.
- [How check works](docs/check.md): the throwaway schema, reading query plans, suggestions and output formats.
- [Keeping your databases safe](docs/safety.md): which indexes Narcissus builds, supported SQLite versions, and undoing a change.
- [Using it with migrations](docs/migrations.md)
- [Running it in CI](docs/ci.md)
- [Collations other programs don't have](docs/collations.md): finding them, and converting a copy.

## No warranty

Narcissus is provided "as is", without warranty of any kind, express or implied, as set out in its MIT and Apache-2.0 licences. You use it at your own risk. In no event are the authors or copyright holders liable for any claim, damages or other liability, including loss of or damage to data, arising from the software or its use.

Before you run `apply`, please back up your databases and check that the backups restore, read what `narcissus plan` would change, and try it on a copy first.

## Contributing

Contributions are very welcome; see [CONTRIBUTING.md](CONTRIBUTING.md), and [CHANGELOG.md](CHANGELOG.md) for what's changed. If you find a problem that could put people's data at risk, please report it privately, as [SECURITY.md](SECURITY.md) describes.

## Licence

Licensed under either of

- Apache License, Version 2.0 ([LICENSE-APACHE](./LICENSE-APACHE))
- MIT licence ([LICENSE-MIT](./LICENSE-MIT))

at your option.

Unless you explicitly state otherwise, any contribution intentionally submitted for inclusion in this work, as defined in the Apache-2.0 licence, shall be dual licensed as above, without any additional terms or conditions.
