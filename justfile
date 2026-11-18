# Narcissus task runner. Run inside the dev shell (direnv, or `nix develop -c just ...`).

set shell := ["bash", "-euo", "pipefail", "-c"]

version := `git describe --always --dirty 2>/dev/null || echo dev`

# List the recipes.
default:
    @just --list

# Everything CI checks.
ci: lint test security build

# Lint and check formatting.
lint:
    golangci-lint run ./...

# Format the code.
fmt:
    golangci-lint fmt ./...

# Run the tests with the race detector (which needs cgo).
test:
    CGO_ENABLED=1 go test -race ./...

# Known vulnerabilities in the dependencies, and secrets in the history.
security:
    govulncheck ./...
    gitleaks git --no-banner --redact -v .

# Build the binary to bin/narcissus.
build:
    go build -trimpath -ldflags "-X main.version={{version}}" -o bin/narcissus ./cmd/narcissus

# Run the tests on Linux, through a Nix builder for aarch64-linux.
test-linux:
    nix build .#checks.aarch64-linux.test --no-link -L
