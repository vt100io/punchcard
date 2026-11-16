# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build (runs govulncheck first, then go build)
mise run build

# Check for vulnerabilities only
mise run check

# Run directly
go run .
```

The `build` task in `mise.toml` depends on `check` (govulncheck), so `mise run build` always validates security before compiling. The binary is expected at `bin/punchcard` for deployment (per `unit.toml`).

## Architecture

Punchcard is a single-binary Go CI server. The server is stdlib only; only the TUI (`tui.go`) uses Charm's Bubble Tea and Lip Gloss. It receives GitHub App webhooks and runs `mise` tasks in persistent local workspaces. It's a personal tool: anyone with write access to a repo is trusted. Pull requests are built only when their author has write access (`mayBuild`: GitHub's collaborator permission API, or `author_association` without an App), so strangers' PRs on public repos never run.

**Request flow:**
1. `webhook.go` — verifies HMAC-SHA256 (`PUNCHCARD_SECRET`), handles `push` to the repo's default branch and `pull_request` (`opened`/`synchronize`/`reopened`), and calls `Schedule(job)`. PR jobs always use the base repo.
2. `build.go` — one queue and one worker, so a workspace is never used by two jobs at once. `Schedule` drops queued jobs for the same branch or PR (`supersedeKey`); nightly jobs are never dropped. `runJob` does: `checkout` (clone or fetch, plus `pull/N/head` for PRs, then `reset --hard <sha>`, `git clean -fd`, `mise trust`), checks the task exists (`mise tasks info`; if not, the repo hasn't opted in and no status is posted), then posts pending, `installTools` (`mise install`) and `mise run <task>`. Output is streamed to slog and the build log, and captured; commit statuses link to the log. A passing build hashes each file it wrote to `bin/` (`binaries`), records them in history and posts each as a `punchcard/sha256 bin/<name>` status before the success status.
3. `scheduler.go` + `report.go` — at startup, every repo the App is installed on (`GitHubClient.Repos`) without a workspace gets a `first` build of its default branch. Nightly, every App repo plus every workspace gets a `nightly` job through the same queue: the `nightly` task, or `ci` without one, on the default branch's tip, with its status posted there. Then a text report.
4. `github.go` — GitHub App client: looks up the installation for each repo (cached) and caches installation tokens per installation. A nil client means no App: SSH clone, no statuses.
5. `logs.go` — build logs in `$STATE_DIRECTORY/logs/<random id>.log`, served at `GET /builds/{id}` and pruned after 30 days. The public base URL comes from the webhook request (`Host` + `X-Forwarded-Proto`).
6. `history.go` — every finished or dropped build is appended to `$STATE_DIRECTORY/history.jsonl` and kept in memory (`History.Record`/`Recent`); `punchcard history [repo]` prints it. Not served over HTTP, because an index would reveal the log IDs.
7. `feed.go` — an in-process broadcast of build events (queued, dropped, started, each log line, finished) for live viewers such as a future TUI. `Publish` never blocks; slow subscribers miss events.
8. `control.go` — the control socket `$STATE_DIRECTORY/punchcard.sock` (mode 0600, no auth): `GET /state` (`Snapshot`: running build, queue, recent history), `GET /events` (the feed as NDJSON; log lines carry their line number `N`), `GET /builds/{id}`.
9. `tui.go` — `punchcard tui`, a separate process that reads the control socket. On any non-line event it re-reads `/state` rather than patching state, so missed events can't make it drift; log lines are merged by line number with the log file.
10. `main.go` — config, the `history` and `tui` subcommands, systemd socket activation, notify and watchdog.

**Convention:** every repo defines a `ci` mise task (the `ciTask` const). There are no per-project settings.

**Configuration:** `PUNCHCARD_SECRET` (required), `PUNCHCARD_GITHUB_APP_ID` + `GITHUB_APP_PRIVATE_KEY` (App), `PUNCHCARD_NIGHTLY_TIME` and `PUNCHCARD_URL` (optional). Workspaces and reports go under `$STATE_DIRECTORY` (default `~/.local/state/punchcard`). Secrets are read from `$CREDENTIALS_DIRECTORY` first. `unit.toml` is the deploy config, and `secrets.sh` exports secrets at deploy time.
