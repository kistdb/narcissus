# Contributing

Thank you for helping. Narcissus is a small Go command-line tool for SQLite, and small, focused changes are the easiest to review.

## Setting up

The development shell is a Nix flake. With direnv, run `direnv allow`; without it, put `nix develop -c` in front of each command. The shell provides Go, golangci-lint, govulncheck, gitleaks and just.

```sh
just build       # bin/narcissus
just test        # the tests, with the race detector
just lint        # lint and formatting checks
just fmt         # format the code
just ci          # everything CI checks: lint, tests, vulnerability and secret scans, build
just test-linux  # the tests on Linux, through a Nix builder for aarch64-linux
just test-compat # changed databases checked by older SQLite versions, built from sqlite.org
```

Please run `just ci` before opening a pull request. GitHub Actions also runs the tests on Linux, macOS and Windows for every pull request.

## Releases

Versions follow [Semantic Versioning](https://semver.org/). To release, move the "Unreleased" notes in [CHANGELOG.md](CHANGELOG.md) under the new version, merge that, then push a signed tag such as `v0.1.0`. The release workflow tests, builds for Linux, macOS and Windows, and publishes the files with checksums and build provenance.

## Code

- Plain, idiomatic and correct Go is preferred to clever Go.
- Comments only where they help, and never more than two short lines.
- Every bug fix comes with a test that fails without it.
- No cgo: SQLite comes from [`ncruces/go-sqlite3`](https://github.com/ncruces/go-sqlite3).
- Anything that writes to a user's database goes through `internal/sqlite` (`Open`, `ExecIndexChange`), which guards against changing anything but indexes.

| Package | What it does |
|---|---|
| `internal/sqltext` | Reads SQL without a database: tokens, statements and name rules. |
| `internal/index` | Parses `indexes.sql`, compares indexes and plans changes. |
| `internal/sqlite` | Opens databases safely, runs scripts and reads schemas. |
| `internal/scratch` | Builds the in-memory database `check` uses. |
| `internal/check` | Reads query plans and suggests indexes. |
| `internal/fleet` | Runs `plan` and `apply` across many databases. |
| `cmd/narcissus` | The command line. |

## Docs

Docs and messages are written in British English: clear, concise and friendly, honest about risks without being alarming.

## Commits and pull requests

For anything bigger than a small fix, please open an issue first, so we can agree the approach before you spend time on it. Reviews happen when time allows, so please bear with us.

- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/): `feat: ...`, `fix: ...`, `docs: ...`.
- For anything a user would notice, add a line under "Unreleased" in [CHANGELOG.md](CHANGELOG.md).
- Pull requests are squash-merged.

By contributing, you agree that your work is licensed as described in the [README](README.md#licence).
