# Getting started

## Install

**Download a release.** The [releases page](https://github.com/kistdb/narcissus/releases/latest) has a ready-made `narcissus` for Linux, macOS and Windows, on Intel and Arm. Unpack it somewhere on your `PATH`, then:

```sh
narcissus version
```

Each release lists SHA-256 checksums in `checksums.txt`, and GitHub keeps a signed record of how each file was built, which you can check with `gh attestation verify narcissus_0.1.0_linux_amd64.tar.gz --repo kistdb/narcissus`.

**Or build it with Go** 1.27 or later:

```sh
go install github.com/kistdb/narcissus/cmd/narcissus@latest
```

Go puts the binary in `$(go env GOPATH)/bin`. If `narcissus version` isn't found, add that folder to your `PATH`.

### On macOS

The macOS downloads aren't notarised by Apple yet, so when one comes from a web browser, macOS stops it on first run with "Apple could not verify 'narcissus' is free of malware". To run it, either:

- open **System Settings → Privacy & Security** and choose **Open Anyway**; or
- clear the download mark in Terminal, from the folder you unpacked:

  ```sh
  xattr -d com.apple.quarantine narcissus
  ```

Downloading with `curl` or `gh release download`, or installing with `go install`, avoids the message altogether. On Windows, opening it from File Explorer can show a similar SmartScreen warning; choose **More info**, then **Run anyway**.

## Your first check

1. **Write the starting files from a database you already have:**

   ```sh
   narcissus init app.db
   ```

   This writes three files for you to commit:

   | File | Holds |
   |---|---|
   | `schema.sql` | Your tables, views and triggers. |
   | `indexes.sql` | Every index you want, as `CREATE INDEX` statements. From now on, this file is where your indexes live. |
   | `queries.sql` | The queries that matter to you. This one starts empty for you to fill in. |

2. **Add your important queries** to `queries.sql`, each with a name in a comment above it:

   ```sql
   -- name: recent_orders
   SELECT * FROM orders WHERE customer_id = ? ORDER BY created_at DESC LIMIT 20;
   ```

3. **Check them:**

   ```sh
   narcissus check
   ```

4. **Act on what it finds,** as described below.

5. **See what would change in your database, then apply it:**

   ```sh
   narcissus plan app.db
   narcissus apply app.db
   ```

To try Narcissus without a database of your own, run `narcissus check` in the [`example/`](../example) folder. It fails one query on purpose, so you can see what a problem looks like.

## When check finds a problem

For each query that would be slow, you have three choices:

- **Add the suggested index.** Copy the `suggest:` line into `indexes.sql` and run `check` again. When you're ready, `plan` and `apply` will add it to your databases.
- **Rewrite the query** so it can use an index you already have.
- **Accept it,** if reading the whole table is what you want (an export, say). Put this above the query:

  ```sql
  -- name: export_all
  -- narcissus: allow full-scan
  SELECT * FROM orders;
  ```

Sometimes no single index would help, for example when a query filters on a calculation. Narcissus will say so, and you can rewrite the query or allow the rule.

`check` also mentions indexes that no query uses. If you'd like to keep one anyway, put `-- narcissus: allow unused` above it in `indexes.sql`.

## Next steps

- [Commands](commands.md) covers every flag.
- [Keeping your databases safe](safety.md) explains which indexes Narcissus builds, and why it declines some.
- [Running it in CI](ci.md) checks your queries on every pull request.
