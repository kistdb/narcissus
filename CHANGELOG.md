# Changelog

All notable changes to Narcissus are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## Unreleased

### Added

- `check` finds queries whose plans read a table without an index, sort in a temporary structure or build a temporary index, and suggests indexes that would help, including a pair of indexes for joins. Output as text, GitHub annotations or JSON.
- `plan` and `apply` bring the indexes of one database, or of many matched by a glob, in line with `indexes.sql`.
- `plan -exit-code` exits with status 3 when a database needs changes, for catching drift.
- `init` writes `schema.sql`, `indexes.sql` and a starter `queries.sql` from an existing database.
- `check` reads its schema from a SQL file, a database file (with its statistics) or a migrations folder: plain, golang-migrate, dbmate, goose, sql-migrate or Prisma.
- `-- narcissus: allow unused` above an index keeps `check` from reporting it as unused; indexes that support a foreign key are never reported.
- `collations` lists collations other SQLite programs don't have, such as Android's `LOCALIZED`, and NOCASE indexes an app built with its own NOCASE. `convert` writes a copy of a database using built-in collations instead: a dry run unless `-write` is given, refusing when rows would collide or foreign keys would break, copying one consistent moment, and keeping rows, rowids and settings exactly; the copy is compared value by value with the original and must pass `integrity_check`.
- `narcissus version` shows the version of SQLite built in.
- A no-warranty notice in the README and the usage text.

### Safety

- `apply` only ever creates and drops indexes, one statement at a time, and SQLite itself is set to refuse anything else.
- Every change is checked before any is made, and `plan` runs the same checks. If any change in a database would be refused, nothing in it is changed.
- Each database is changed in one transaction, so a failure, Ctrl-C, a closed terminal or a closed pipe leaves it as it was.
- Text columns holding decimal numbers, and column defaults of more than 15 digits, are declined where they'd be converted. Databases in SQLite's checksum format are declined.
- `init` lists existing indexes Narcissus wouldn't build itself.
- Supported app SQLite: the versions shipped by operating systems with free, regular support, from RHEL 8's 3.26; each release is tested against them. `RTRIM` isn't built, as it sorts differently before 3.29.
- `apply` builds only indexes every SQLite computes the same way: columns, and a WHERE comparing columns with whole numbers, text or `NULL`; with `-allow-collations-and-functions`, also `NOCASE`, `lower()`, `upper()` and `trim()` of text columns. Anything else is declined with a note to create it from the app. Numeric columns holding text, unusual column defaults and UTF-16 text conversions are declined as well.
- A double-quoted name in `indexes.sql` must be a real column; it's never read as text.
- `-prune` keeps UNIQUE indexes unless `-prune-unique` is given, which replacing a UNIQUE index also needs, and won't run when `indexes.sql` is empty.
- Narcissus opens only regular SQLite files with a single name, checks it still has the same file after SQLite opens it, and never creates a database. Journal and WAL files take the database's permissions. `check` can't write to any file, and `init` won't overwrite the database it reads.
- Changing databases is supported on Linux, macOS and Windows; BSD and illumos are declined for now.
