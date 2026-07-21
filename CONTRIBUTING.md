# Contributing to KARMAX

Thanks for your interest! KARMAX is a personal AI orchestration daemon; it runs
with broad access to a machine and accounts, so contributions should be careful
and well-scoped.

## Development setup

Requirements: **Go 1.24+** and a C toolchain (CGO is required — memory/state use
SQLite via `mattn/go-sqlite3`).

```bash
git clone https://github.com/MelloB1989/karmax
cd karmax
make build          # or: CGO_ENABLED=1 go build -o karmax ./cmd/karmax
```

Configuration lives in `karmax.yaml` (gitignored) and `.env` (gitignored). Copy
the documented example, point it at test accounts, and never commit real config.

## Before you open a PR

```bash
gofmt -l .          # must print nothing
go vet ./...        # must be clean
go build ./...      # must succeed
go test ./...       # if you touched anything with tests
```

CI runs the same checks on every PR (`.github/workflows/ci.yml`).

## Architecture notes

- **Orchestrator only.** The main agent decides and delegates; heavy work goes
  to harnesses (`claude_code`). Keep new capabilities as tools or loops, not
  hardcoded automation.
- **Loops** are authored with the `loopkit` SDK (`pkg/loopkit`) and published to
  the public [karmax-loops](https://github.com/MelloB1989/karmax-loops)
  registry — prefer a loop over a hidden goroutine.
- **Human-in-the-loop.** Anything money-spending, publicly-posting, or
  destructive must go through the approval flow, never act directly.
- **No secrets in code or fixtures**, ever.

## Reporting bugs & requesting features

Use the templates in `.github/ISSUE_TEMPLATE`. For security issues, follow
[SECURITY.md](SECURITY.md) — do not open a public issue.

## License

By contributing, you agree your contributions are licensed under the
[MIT License](LICENSE).
