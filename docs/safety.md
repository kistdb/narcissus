# Keeping your databases safe

`apply` is the only command that changes your databases, and it only ever creates and drops indexes, never your tables or rows. This page explains how Narcissus decides what it's safe to build, and what to know before you run it.

## Which indexes Narcissus builds

Narcissus builds indexes with its own copy of SQLite (run `narcissus version` to see which), and your app may use a different version or build. An index is only correct if both compute its keys the same way, so Narcissus builds only indexes that every SQLite computes identically:

- **Columns, in any order, ascending or descending.** For example `(customer_id, created_at DESC)`.
- **A partial index's WHERE** that compares columns with whole numbers, text or `NULL`, using `=`, `<`, `>`, `IS NULL`, `IN`, `BETWEEN`, `AND`, `OR` and `NOT`. Text is only compared with text columns. For example `WHERE deleted_at IS NULL AND status IN ('open', 'held')`.
- **With `-allow-collations-and-functions`,** also the `NOCASE` collation, and `lower()`, `upper()` and `trim()` of text columns. An app can replace these with its own versions, which is why they need the flag; if your app uses SQLite's standard ones, as most do, the flag is safe. For example `(lower(email))` or `(email COLLATE NOCASE)`.

A few cases are declined because of the data they'd hold: a whole number compared with a numeric column that holds text in some rows, a column whose default is an unusual number (such as `1e-300`) that older rows don't store, and text comparisons or `lower()`, `upper()` and `trim()` in a UTF-16 database. Write `1` and `0` rather than `TRUE` and `FALSE`, which older SQLite doesn't know.

Anything else is declined, with a note saying why: other functions, JSON, arithmetic, casts, decimal numbers in a WHERE, and virtual generated columns. Different SQLite versions can compute those differently, sometimes only in the last digit of a decimal number, and that's enough for an app to miss rows or reject valid ones. Please create those indexes from your app, so they're built with its own SQLite. `check` still checks queries against them.

## Supported SQLite versions

Narcissus supports the SQLite your app runs on as long as it comes from an operating system release that still has free, regular support from its maker. When a release reaches its end of life, or is only kept going by paid extended support, its SQLite is no longer covered. Today that means:

| Operating system | SQLite |
|---|---|
| Red Hat Enterprise Linux 8, and Rocky and Alma Linux 8 | 3.26 |
| Red Hat Enterprise Linux 9 | 3.34 |
| Ubuntu 22.04 | 3.37 |
| Debian 12 | 3.40 |
| Ubuntu 24.04 | 3.45 |
| Debian 13 | 3.46 |
| macOS, and distributions that track recent SQLite | newer |

Each release is tested against these versions, with them acting as the app. Check yours with `sqlite3 --version`, or `SELECT sqlite_version()` from your app. If your app bundles its own SQLite, as many language drivers do, that's the version that counts.

The `RTRIM` collation isn't built: SQLite changed how it sorts some text in 3.29, so an index built by a newer SQLite could confuse 3.26.

## Undoing a change

Indexes hold no data of their own, so creating, dropping or rebuilding one never loses rows.

- **To remove an index,** delete its line from `indexes.sql`, then run `plan -prune` and `apply -prune`. These drop every index that isn't in the file, so do read the plan first. You can also `DROP INDEX` it yourself.
- **To restore an index you changed,** put the old line back and run `apply`.

While a UNIQUE index is gone, duplicate rows can slip in, so please take extra care with those.

## Good to know

- **SQLite only.** Narcissus relies on SQLite's own planner and parser, on purpose.
- **Platforms.** Linux, macOS and Windows. On BSD and illumos, `apply` declines to change databases for now, because file locking there may not match other SQLite programs.
- **Database files.** Narcissus opens only files that start with the SQLite header, and never creates a database. A file with two names (hard links) would have two journals, which isn't safe, so Narcissus declines it. Symlinks are fine, and a file reached by several paths is handled once.
- **Side files.** Like any SQLite program, Narcissus may leave `-wal` and `-shm` files beside a database in WAL mode. Please run it as the user that owns your databases, not as root, so those files belong to the right user.
- **Checksummed databases.** Databases that reserve 8 bytes per page, as SQLite's checksum extension does, aren't supported yet, so Narcissus declines to open them.
- **Virtual tables.** FTS5 and R*Tree are built in. FTS3 and FTS4 aren't, so `check` can't load a schema that uses them.

And, as the [README](../README.md#no-warranty) says, please keep backups and read the `plan` before you `apply`.
