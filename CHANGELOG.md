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
- WAL databases in read-only folders open read-only, as immutable, for `check`, `plan` and `init`.
