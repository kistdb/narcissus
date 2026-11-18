# Contributing

Thanks for helping. Narcissus is a small Go CLI for SQLite; keep changes focused.

## Set up

The dev shell is a Nix flake. With direnv, run `direnv allow`; without it, prefix commands with `nix develop -c`. It provides Go, golangci-lint, govulncheck, gitleaks and just.

```sh
just build   # bin/narcissus
just test    # tests with the race detector
just ci      # lint, tests, vulnerability and secret scans, build
```

Run `just ci` before opening a pull request.

## Code

- Idiomatic, correct Go. Prefer plain code to clever code.
- Comments only where they're needed, and never more than two short lines.
- Every bug fix comes with a test that fails without it.
- No cgo: SQLite comes from [`ncruces/go-sqlite3`](https://github.com/ncruces/go-sqlite3).

| Package | Does |
|---|---|
| `internal/sqltext` | Lexing, statement splitting and name rules, without a database. |
| `internal/index` | Parsing `indexes.sql`, comparing indexes and planning changes. |
| `internal/sqlite` | Opening databases, running scripts, reading schemas. |
| `internal/scratch` | Building the in-memory database `check` uses. |
| `internal/check` | Reading query plans and suggesting indexes. |
| `internal/fleet` | `plan` and `apply` across many databases. |
| `cmd/narcissus` | The command line. |

## Commits and pull requests

- Write commit messages as [Conventional Commits](https://www.conventionalcommits.org/): `feat: ...`, `fix: ...`, `docs: ...`.
- Add a line under "Unreleased" in [CHANGELOG.md](CHANGELOG.md) for anything a user would notice.
- Pull requests are squash-merged.

By contributing you agree that your work is licensed as described in the [README](README.md#license).
