# Changelog

All notable changes to Narcissus are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## Unreleased

### Added

- `check`: fails queries whose plans read a table without an index, sort in a temporary b-tree or build an automatic index, and suggests indexes that fix them. Text, GitHub Actions and JSON output.
- `plan` and `apply`: bring the indexes of one database, or of many matched by a glob, in line with `indexes.sql`, each database in one transaction.
- `plan -exit-code`: exits 3 when a database needs changes, for drift checks.
- `init`: writes `schema.sql` and `indexes.sql` from an existing database.
- Schema sources for `check`: a SQL file, a database file with its planner statistics, or a migrations folder (plain, golang-migrate, dbmate, goose).
- A no-warranty notice in the README and the command's usage text.
- `apply` runs only `CREATE INDEX` and `DROP INDEX`, one statement at a time, under SQLite's authorizer.
- `-prune` drops only non-UNIQUE indexes; `-prune-unique` drops UNIQUE ones too. `-prune` is refused when `indexes.sql` is empty.
- `apply` refuses indexes that depend on collations, functions, function-backed operators or virtual generated columns an app can redefine, unless `-allow-collations-and-functions` is given. It checks what SQLite reports the index reads, before building it.
- Replacing a UNIQUE index with one that isn't needs `-prune -prune-unique`.
- Databases with more than one hard link are refused; each file is handled once however many paths lead to it. Glob matches that aren't regular files are reported, and unreadable folders are errors.
- `check` refuses `ATTACH` and `VACUUM INTO` in schema sources.
- After opening, Narcissus checks that the file SQLite opened is the one it checked, with one name, so a file swapped in between is refused; opening gives up on Ctrl-C or after the busy timeout and 30s, instead of hanging on a swapped-in FIFO.
- Only regular files with the SQLite header are opened; journal and WAL files take the database's permissions; `-busy-timeout` applies from the first read.
- WAL snapshots in read-only folders open as immutable when they have no journal or WAL content.
- Changing databases is supported on Linux, macOS and Windows; BSD and illumos are refused for now.
