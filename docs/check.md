# How check works

`narcissus check` answers one question for each query in `queries.sql`: will SQLite find the rows it needs through an index, or will it read the whole table?

## 1. A throwaway copy of your schema

`check` creates an empty database in memory and loads your tables into it from `-schema`:

| Source | What happens |
|---|---|
| A SQL file | Its statements run in order. |
| A database file | Its tables, views and triggers are copied, but never its rows. If the database has been through `ANALYZE`, its statistics are copied too, so SQLite plans as it would in production. |
| A migrations folder | The migrations run in version order. Plain `.sql` files, golang-migrate (`*.up.sql`), dbmate (`-- migrate:up`), goose (`-- +goose Up`), sql-migrate (`-- +migrate Up`) and Prisma (`*/migration.sql`) all work. For other tools, point `-schema` at a migrated database file instead. |

`ATTACH` and `VACUUM INTO` aren't allowed in a schema source, so `check` can never touch another file.

Then the indexes your schema created are set aside, and the ones in `indexes.sql` are built instead. If your schema created an index that `indexes.sql` doesn't mention, `check` lets you know, so nothing disappears quietly.

## 2. Reading each query's plan

For each query, `check` asks SQLite for its plan (`EXPLAIN QUERY PLAN`) and looks for three kinds of step:

| Rule | Plan step | Why it can be slow |
|---|---|---|
| `full-scan` | `SCAN orders` | Reads every row of the table. |
| `temp-btree` | `USE TEMP B-TREE FOR ORDER BY` | Sorts or groups the rows every time the query runs. |
| `automatic-index` | `SEARCH c USING AUTOMATIC COVERING INDEX` | Builds a temporary index every time the query runs. |

Your queries are never run, only planned, so their contents don't matter beyond the SQL itself.

Some scans are perfectly fine and aren't reported: those of subqueries, `WITH` clauses and `VALUES` lists, and those that walk an index in order (`SCAN orders USING INDEX ...`, as `ORDER BY ... LIMIT` does). Two honest limits to know about:

- Walking an index can still visit every row, for example when there's no `LIMIT`, so a pass isn't a promise of speed.
- `ORDER BY id LIMIT 20` on a table's own key shows as `SCAN orders` and fails, though it's quick. Allow `full-scan` for that query.

When reading the whole table is what you want, allow it for that one query:

```sql
-- name: export_all
-- narcissus: allow full-scan
SELECT * FROM orders;
```

The comment must sit directly above the query, with no blank line between. Several rules can share one line: `-- narcissus: allow full-scan, temp-btree`.

## 3. Suggesting indexes

For a failing query, `check` tries out indexes on the columns the query reads:

- It scores each one by the problems left in the new plan. Reading a whole index counts as a problem too, so such an index is only suggested when it also removes a sort.
- It tries single columns first, then grows the best few by one column at a time, up to three columns.
- If no single index helps, as in a join that needs one on each side, it tries pairs across two tables.
- It suggests the best, adds it, and repeats, up to three suggestions per query.

All of this happens in a part of the transaction that's undone straight afterwards, so one query's suggestions never affect the next. Expression and partial indexes are never suggested, though both are checked.

## Realistic plans

Without statistics, SQLite guesses how big each table is. Missing indexes still show up reliably, but which of two good indexes SQLite prefers can differ from production. For production-like plans, point `-schema` at a copy of a real database that has been through `ANALYZE`:

```sh
sqlite3 app.db ".backup snapshot.db"
narcissus check -schema snapshot.db
```

Only the schema and statistics are read. If the copy uses WAL mode, SQLite may create `snapshot.db-wal` and `snapshot.db-shm` beside it while reading; you might like to keep them out of git:

```gitignore
*.db-wal
*.db-shm
```

## Output formats

`-format text` is the default. `-format github` writes annotations that GitHub shows on the pull request, next to the query. `-format json` writes a report for scripts:

```json
{
  "failed": true,
  "queries": [
    {
      "name": "recent_orders",
      "file": "queries.sql",
      "line": 5,
      "problems": [{ "rule": "full-scan", "detail": "SCAN orders" }],
      "suggest": ["CREATE INDEX orders_customer_id_created_at ON orders (customer_id, created_at);"]
    }
  ],
  "unused_indexes": [{ "name": "orders_total", "line": 2 }],
  "ignored_schema_indexes": []
}
```

A query that SQLite couldn't plan has an `error` field instead of `problems`.
