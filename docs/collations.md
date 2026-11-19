# Collations other programs don't have

A SQLite database records the *name* of each collation it uses, such as `LOCALIZED`, and keeps its indexes in the order that collation produced. The code that compares text lives in the app that registered it: Android's `LOCALIZED` and `UNICODE`, ICU's collations, or one an app wrote itself.

Open such a database anywhere else and every statement that compares or sorts that text fails with "no such collation sequence". That includes `ORDER BY`, `WHERE name = …` and even `PRAGMA integrity_check`. SQLite's three built-in collations, `BINARY`, `NOCASE` and `RTRIM`, don't have this problem, because every SQLite has them as its own code.

Narcissus can find these collations, and write a copy of the database that uses built-in ones instead.

## Finding them

```sh
narcissus collations app.db
```

```
app.db
  table people, column name: LOCALIZED
  view sorted: LOCALIZED
  index people_city, key city: LOCALIZED
Other SQLite programs can't compare or sort by collations they don't have. narcissus convert can write a copy that uses built-in ones.
```

It also points out a `NOCASE` or `RTRIM` index whose order doesn't match SQLite's own, which happens when the app that built it replaced that collation with its own version. Another program that writes to such a database could put the index out of order.

`collations` doesn't change the database. Like any SQLite program, though, opening a WAL-mode database creates its `-wal` and `-shm` files if they're missing, and leaves them there, with the same permissions as the database. It exits with status 1 when it finds anything, so it can guard a CI job, and `-format json` gives a report for scripts. You can give it several databases at once.

## Converting a copy

SQLite can't change a column's collation in place, so Narcissus writes a **new file** with the same tables, rows, indexes, views and triggers, using built-in collations. Your original database is never changed.

```sh
narcissus convert -to nocase app.db app-converted.db          # a dry run: shows what would happen
narcissus convert -to nocase -write app.db app-converted.db   # writes app-converted.db
```

`convert` always starts with a dry run. Nothing is written unless you add `-write`.

- **Choosing collations:** `-to` sets the built-in collation (`binary`, `nocase` or `rtrim`) for every other one. `-map` names them one by one, as in `-map LOCALIZED=nocase,UNICODE=binary`; `-map` wins where both apply. There's no default, so please give at least one of them.
- **Rows that would become equal:** if a UNIQUE index or primary key holds values that are different now but would be equal under the new collation, such as `'Zoe'` and `'zoe'` moving to `NOCASE`, Narcissus lists them and doesn't convert. Please resolve those rows first, then run it again.
- **Foreign keys:** in the same way, if a row wouldn't match its parent row under the new collation, Narcissus says how many and doesn't convert. Without the app's collation it can't tell whether a row already had no parent, so those count too.
- **What changes in meaning:** the dry run says how the new collation compares text. For example, `NOCASE` ignores the case of A to Z only, so `'é'` and `'É'` stay different. Add `-queries queries.sql` to see which of your queries read the columns concerned.
- **What's kept:** every row and its rowid, byte for byte, `AUTOINCREMENT` counters, generated columns, `WITHOUT ROWID` tables, FTS5 and R*Tree tables, foreign keys, the page size, text encoding, `user_version`, `application_id`, WAL mode and the file's permissions. Query planner statistics aren't copied; the dry run says so, and you can run `ANALYZE` on the new file.
- **One moment in time:** the copy is of the database as it was when `convert` started, even if your app writes to it meanwhile. In WAL mode the app carries on as usual; otherwise it waits to write until `convert` has finished.
- **Checks:** before keeping the new file, Narcissus compares every stored value in every table with the original, and checks that it uses only built-in collations, passes `PRAGMA integrity_check`, and has no new broken foreign keys. If anything differs, the file isn't kept. It only appears under its name once it has passed.
- **Problems found early:** the dry run builds the new schema in memory first, so anything it can't create, such as an index that calls a function only your app has, shows up before any copying.

Some things aren't converted, and Narcissus says so rather than guess:

- FTS3 and FTS4 tables;
- UNIQUE indexes on expressions, whose rows it can't check for collisions;
- a UNIQUE index whose `WHERE` compares a column whose collation changes, as the rows it covers could change;
- a generated column or `CHECK` constraint that compares such a column, as its result could change;
- a UNIQUE index whose app replaced `NOCASE` or `RTRIM` with its own version, as SQLite's could disagree about which rows are equal.

## After converting

Point your app at the new file once you're happy with it. If the app registered its own collation for a reason, such as sorting names in a particular language, that sorting now follows the built-in collation instead, so please check the screens and queries that depend on it.
