# Running Narcissus in CI

## Checking queries on every pull request

`check` needs no database, only your SQL files, so it runs anywhere.

```yaml
name: queries
on: pull_request
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4.4.0
      - uses: actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff # v5.6.0
        with:
          go-version: "1.27.x"
      - run: go install github.com/kistdb/narcissus/cmd/narcissus@v0.1.0
      - run: narcissus check -format github
```

`-format github` puts each problem on the pull request as a note beside the query in `queries.sql`.

If your tables come from migrations, point `check` at the folder:

```sh
narcissus check -schema db/migrations
```

On other CI systems, use the default text output, or `-format json` for a report a script can read. `check` exits with status 1 when a query fails.

Installing a fixed version, as above, means every run uses the same Narcissus; move it on when you're ready for a new release.

## Catching drift

`plan -exit-code` exits with status 3 when a database's indexes don't match `indexes.sql`, which suits a scheduled job that can reach your databases:

```sh
narcissus plan -exit-code 'tenants/*.db'
```

Indexes that are in a database but not in `indexes.sql` only count as drift if you add `-prune`. A database that can't be read gives status 1.

## Applying on deploy

`apply` is the one step that changes your databases. Please back them up first, and run `plan` earlier in the same job, so the log shows what was about to change. Narcissus comes with no warranty (see [No warranty](../README.md#no-warranty)).

Run `apply` in the deploy step, after your migrations:

```sh
narcissus plan 'data/tenants/*.db'
narcissus apply 'data/tenants/*.db'
```

- **Collations and functions:** if your indexes use `COLLATE NOCASE` or `lower()`, and your app uses SQLite's standard versions, add `-allow-collations-and-functions`. [Keeping your databases safe](safety.md#which-indexes-narcissus-builds) explains which indexes Narcissus builds.
- **One transaction each:** each database is changed on its own. One that fails is reported and gives exit status 1, without stopping the rest.
- **Locking:** building an index on a large table holds SQLite's write lock while it runs, so big changes are best made at a quiet time.
