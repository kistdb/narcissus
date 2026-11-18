# Running Narcissus in CI

## Check queries on every pull request

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
          go-version: "1.27"
      - run: go install github.com/kistdb/narcissus/cmd/narcissus@latest
      - run: narcissus check -format github
```

`-format github` turns each problem into an annotation on the pull request, next to the query in `queries.sql`.

If your tables come from migrations, point `check` at the folder:

```sh
narcissus check -schema db/migrations
```

For other CI systems, use the default text output, or `-format json` for a report a script can read. `check` exits 1 when a query fails.

## Catch drift

`plan -exit-code` exits 3 when a database's indexes don't match `indexes.sql`. Use it in a scheduled job that can reach your databases:

```sh
narcissus plan -exit-code 'tenants/*.db'
```

## Apply on deploy

`apply` changes your databases, with no warranty (see [No warranty](../README.md#no-warranty)). Back them up first, and run `plan` in the same job before it.

Run `apply` in the deploy step, after migrations:

```sh
narcissus apply 'data/tenants/*.db'
```

Each database changes in its own transaction. A failed one is reported, makes the exit status 1, and doesn't stop the rest. Building an index on a large table holds SQLite's write lock while it runs, so schedule big changes for quiet times.
