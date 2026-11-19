# Using it with migrations

You don't need to change your old migrations.

1. Run `narcissus init` on a database that's had all your migrations applied, and commit `indexes.sql`.
2. Point `check` at your migrations folder with `-schema db/migrations`. Narcissus reads plain `.sql` files, golang-migrate, dbmate, goose, sql-migrate and Prisma. For anything else, point it at a migrated database file.
3. Add new indexes to `indexes.sql` only, not to new migrations.
4. Run `narcissus apply` straight after your migrations. If a migration rebuilds a table, which drops its indexes, `apply` puts them back.
