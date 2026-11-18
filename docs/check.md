# How check works

`narcissus check` answers one question for each query in `queries.sql`: will SQLite find the rows through an index, or read the whole table?

## 1. Build a scratch database

`check` creates an empty database in memory and loads the tables from `-schema`:

| Source | What happens |
|---|---|
| A SQL file | Its statements run in order. |
| A database file | Its tables, views and triggers are copied, never its rows. Its planner statistics (`sqlite_stat1`, `sqlite_stat4`) are copied too. |
| A migrations folder | The `.sql` files run in version order. Plain files, golang-migrate `*.up.sql` files, and the up sections of dbmate (`-- migrate:up`) and goose (`-- +goose Up`) files all work. |

Then every index the schema created is dropped, and the indexes in `indexes.sql` are created instead. `check` warns about each dropped index that `indexes.sql` doesn't declare, so nothing disappears silently.

## 2. Read each query's plan

For each query, `check` runs `EXPLAIN QUERY PLAN` and looks for three kinds of step:

| Rule | Plan step | Why it's slow |
|---|---|---|
| `full-scan` | `SCAN orders` | Reads every row of the table. |
| `temp-btree` | `USE TEMP B-TREE FOR ORDER BY` | Sorts or groups the rows on every run. |
| `automatic-index` | `SEARCH c USING AUTOMATIC COVERING INDEX` | Builds a throwaway index on every run. |

Some scans are fine: those of subqueries, CTEs, views and `VALUES` lists, and those that walk an index (`SCAN orders USING INDEX ...`, as `ORDER BY ... LIMIT` does). Aliases such as `FROM orders AS o` resolve to their table.

Sometimes a slow plan is the right one, as in an export that reads everything. Allow it for that one query:

```sql
-- name: export_all
-- narcissus: allow full-scan
SELECT * FROM orders;
```

## 3. Suggest indexes

For a failing query, `check` tries indexes on the columns the query reads:

- It measures each candidate by the problems left in the new plan. Reading a whole index costs as much as a full scan, so an index that only gets read end to end never wins.
- It tries single columns first, then extends the three best by one column at a time, up to three columns.
- It suggests the best candidate, adds it, and repeats, up to three suggestions per query.

All of this happens inside a savepoint that is rolled back, so one query's suggestions never affect the next.

## Realistic plans

With no statistics, SQLite's planner guesses table sizes. Missing indexes still show up reliably, but which of two good indexes it picks can differ from production. For production-like plans, point `-schema` at a copy of a real database:

```sh
narcissus check -schema prod-snapshot.db
```

Only the schema and statistics are read.

If the snapshot uses WAL mode, SQLite creates empty `prod-snapshot.db-wal` and `prod-snapshot.db-shm` files next to it while reading, and a read-only connection can't remove them. Keep them out of git:

```gitignore
*.db-wal
*.db-shm
```

In a read-only folder, where SQLite can't create those files, Narcissus opens the snapshot as immutable instead. That's safe there, since nothing can be writing to it.
