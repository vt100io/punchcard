# Punchcard

**A one-binary CI server that runs the same `mise run ci` you run on your laptop, on a machine that remembers.**

![punchcard tui: a build arrives and streams its log, a newer push replaces a queued one, and a failed build is opened](demo/demo.gif)

Hosted CI runners start from nothing on every build: a fresh machine, fresh tool installs, cold caches. Punchcard keeps a workspace per repo on a machine you own. When you push, it fetches, resets to your exact commit and runs your `ci` task. The toolchain is already installed, `node_modules` and build caches are still there, and the build starts right away.

- **No CI-only language.** No YAML workflows. The pipeline is a [mise](https://mise.jdx.dev) task in your repo, so `mise run ci` on your laptop is exactly what CI runs. If it passes locally it passes in CI, and a CI failure can be debugged on your own machine.
- **mise is the secret weapon.** Toolchains are pinned in the repo, and mise installs each version once and reuses it.
- **Checks on GitHub.** A GitHub App posts pending, passed and failed checks on commits and pull requests, each linked to its build log.
- **A terminal UI.** `punchcard tui` shows live builds in the style of lazygit.
- **Builds without commits.** Installing the App is enough: every repo it can see gets a first build, and every night all of them are built again, including the ones nobody has pushed to in months. A repo that breaks on its own, from a new vulnerability or a vanished download, turns red on GitHub.
- **One convention, no per-project config.** A repo opts in by defining a `ci` task. The server itself needs a webhook secret and the App's credentials.

## What it deliberately isn't

- **Not a sandbox.** There is no container isolation, and builds share a persistent workspace. **Anyone who can open a pull request on a repo where Punchcard is installed can run code on the server.** Only install it on repos where you trust everyone who can open a PR.
- **Not parallel.** One build runs at a time, which keeps workspaces safe and the code simple.
- **Not a platform.** There are no agents, runners, plugins or marketplace.

Punchcard is how I build my own hobby projects: a handful of repos, a machine I trust, and a preference for simple things that do exactly what I expect. It's opinionated, and it's shared because sharing costs nothing, not because it wants to become your CI. If the tradeoffs above are a problem, the general-purpose CI platforms handle them well and you should use those.

## Getting started

You need [mise](https://mise.jdx.dev/), `git` and Go on the machine that will run the builds. It can be a server, or your laptop to try it out.

### 1. Install Punchcard

```bash
go install github.com/vt100io/punchcard@latest
```

This puts a single `punchcard` binary on your `PATH`. `punchcard` runs the server; `punchcard tui` and `punchcard history` look at it.

### 2. Create a GitHub App

The App receives webhooks for your repos, lets Punchcard clone them, and posts the checks. One App covers every repo it's installed on, so you never set up webhooks repo by repo.

1. Make a webhook secret and keep it for step 3:
   ```bash
   openssl rand -hex 32
   ```
2. Create the App at <https://github.com/settings/apps/new> (or under your org's settings):
   - **Webhook URL:** where Punchcard will be reachable, for example `https://ci.example.com/` (step 4 covers a laptop)
   - **Webhook secret:** the value from above
   - **Repository permissions:** Contents *read-only*, Commit statuses *read and write*
   - **Subscribe to events:** Push and Pull request
   - **Where can this App be installed:** Only on this account
3. On the App's page, note the **App ID** and click **Generate a private key**. Save the `.pem` file.
4. Click **Install App** and pick the account. **All repositories** is the simplest choice: only repos with a `ci` task are built (see [Adding a repo](#adding-a-repo)). Choose **Only select repositories** if you want a hard limit.

### 3. Start the server

```bash
export PUNCHCARD_SECRET=<the webhook secret>
export PUNCHCARD_GITHUB_APP_ID=<the App ID>
export GITHUB_APP_PRIVATE_KEY="$(cat path/to/app.private-key.pem)"
punchcard
```

It listens on port 8081 (`PORT` changes it) and logs every webhook and build to the terminal. Workspaces, logs and history go in `~/.local/state/punchcard`.

### 4. Make it reachable from GitHub

GitHub needs to reach the server over HTTPS at the App's webhook URL.

**On a laptop, with [Tailscale Funnel](https://tailscale.com/kb/1223/funnel):** this exposes port 8081 at a public HTTPS address without opening ports on your router.

```bash
tailscale funnel --bg 8081
```

Set the App's webhook URL to the address it prints, such as `https://laptop.tail1234.ts.net/`. Funnel stays on across reboots; `tailscale funnel --https=443 off` turns it off. A laptop is a fine way to try Punchcard, but while it's asleep or offline, webhooks fail. GitHub doesn't retry them: redeliver from the App's settings (Advanced › Recent Deliveries), or push again.

**On a server:** put Punchcard behind any HTTPS reverse proxy, or deploy it with [`unit`](https://github.com/vt100io/unit) (see [Deploying](#deploying)), which sets up systemd and Caddy for you.

### 5. Add a `ci` task to a repo and push

In any repo the App is installed on, add a `mise.toml`:

```toml
[tools]
go = "1.26"

[tasks.ci]
run = "go test ./..."
```

Check that `mise run ci` passes locally, then commit and push to the default branch. Within seconds, the commit gets a pending check on GitHub, then a green or red one. Click it to read the build log.

### 6. Watch it

On the machine running Punchcard:

```bash
punchcard tui
```

## Adding a repo

There is no list of repos to maintain. Punchcard builds any repo that GitHub sends it a webhook for and that has a `ci` task:

1. The App must be installed on the repo. With **All repositories** that's automatic, including repos you create later.
2. Add a `ci` task to the repo's `mise.toml` and push. That's the opt-in.

A repo without a `ci` task is skipped silently: no check is posted and no tools are installed. To stop building a repo, remove its `ci` task. To make a repo unreachable entirely, remove it from the App's repository access at <https://github.com/settings/installations>.

## How it works

1. GitHub sends a webhook on a `push` to the default branch, or on a pull request being opened, updated or reopened.
2. Punchcard verifies the signature against `PUNCHCARD_SECRET`.
3. It brings the repo's workspace to the exact commit: a clone the first time, then `git fetch`, `git reset --hard` and `git clean`. Ignored files such as `node_modules` and build caches survive.
4. If the repo has no `ci` task, it stops there.
5. It posts a pending check, runs `mise install` (near-instant when the tools are cached), then `mise run ci`, and posts the result linked to the build log.

Builds run one at a time. If you push again before a queued build has started, the older build is dropped: only the newest commit on a branch or PR gets built. A build that's already running finishes normally.

## Setting up a repo

`mise run ci` on your laptop is the same command the server runs. Ordering and parallelism go in mise's `depends`, and skipping unchanged work goes in `sources`/`outputs`. Punchcard doesn't add a pipeline language on top.

```toml
[tools]
go = "1.26"

[tasks.ci]
depends = ["lint", "test"]
run = "go build -o bin/myapp ."

[tasks.lint]
run = "go vet ./..."

[tasks.test]
run = "go test ./..."
```

Punchcard runs `mise install` before every build, so a version bump in `[tools]` is picked up on the next build.

Prefer Make, or a shell script? Wrap it:

```toml
[tasks.ci]
run = "make ci"
```

If it runs in `mise`, it runs in Punchcard.

**Tip for Go tools such as `govulncheck`:** run them with `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` rather than listing them under `[tools]`. A mise-installed Go tool is shared by every repo on the build machine and built with whichever Go installed it first, so it can fail on a repo that uses a newer Go. `go run` builds it with the repo's own Go and caches it.

### Environment available to tasks

| Variable | Value |
|---|---|
| `CI` | `true` |
| `PUNCHCARD_EVENT` | `push`, `pull_request`, `nightly` or `first` |
| `PUNCHCARD_REPO` | `owner/repo` |
| `PUNCHCARD_SHA` | The commit being built |
| `PUNCHCARD_BRANCH` | The branch being built |

### Builds without commits

Hosted CI only runs when someone commits, so a repo nobody touches can break without anyone noticing. Punchcard builds repos on its own as well:

- **First build.** At startup, every repo the App is installed on that Punchcard hasn't built yet gets a build of its default branch. Install the App and the repos show up, no push needed.
- **Nightly.** If `PUNCHCARD_NIGHTLY_TIME` is set (as `HH:MM`), Punchcard builds every repo the App can see, plus any it has built before, on the tip of its default branch. It runs the repo's `nightly` task, or `ci` if there isn't one, posts the result as the commit's check, and writes a plain-text report to `$STATE_DIRECTORY/reports/`.

Archived repos are left out, and repos without a `ci` task are skipped as always. Set `PUNCHCARD_URL` so these builds link to their logs: a webhook tells Punchcard its own address, but nothing else does.

## Watching builds

### Terminal UI

```bash
punchcard tui
```

It shows each repo's latest result, the queue (including builds dropped for a newer push), and the log of the selected repo's build, streaming as it runs. Use `↑`/`↓` to pick a repo, `PgUp`/`PgDn` to scroll the log, `g`/`G` for the top or to follow new lines, and `q` to quit.

The TUI talks to the server over a unix socket in the state directory (`punchcard.sock`), readable only by the server's user. To watch a remote server, SSH to it and run it there. Deployed with unit, the server runs as a throwaway user, so run the TUI as root and point it at the server's state directory: `ssh -t root@ci-host STATE_DIRECTORY=/var/lib/punchcard /opt/unit/punchcard/current/bin tui`. In this repo, `mise run tui` does that for the server in `unit.toml`. Quitting the TUI never affects the server, and the TUI reconnects by itself when the server restarts.

### Build logs

Each build's output is written to `$STATE_DIRECTORY/logs/` and served as plain text at `https://<your server>/builds/<id>`. That's the link on the commit check, and you can reload it to follow a build in progress. IDs are random, so only people who can see the commit can find its log. Logs are deleted after 30 days.

### Build history

Every build, including dropped and skipped ones, is appended as one line of JSON to `$STATE_DIRECTORY/history.jsonl`. List recent builds with:

```bash
punchcard history                   # the last 30 builds
punchcard history vt100io/punchcard  # one repo
```

Each entry records when the build was queued, started and finished, so `punchcard history` shows how long each build waited for the worker as well as how long it took. A growing wait is the sign that one build at a time is no longer enough.

History is not served over HTTP: an index of builds would reveal every log ID.

## Configuration reference

| Setting | Required | Purpose |
|---|---|---|
| `PUNCHCARD_SECRET` | yes | Webhook secret, the same value as in the App settings |
| `PUNCHCARD_GITHUB_APP_ID` | yes, with GitHub | The App's ID |
| `GITHUB_APP_PRIVATE_KEY` | yes, with GitHub | The App's private key (PEM) |
| `PUNCHCARD_NIGHTLY_TIME` | no | `HH:MM` to build every repo each night |
| `PUNCHCARD_URL` | no | This server's public URL, for log links on nightly and first builds |
| `PORT` | no | Port to listen on, default 8081. Ignored under systemd socket activation |

Secrets are read from `$CREDENTIALS_DIRECTORY` (systemd `LoadCredential`) if available, otherwise from the environment. Workspaces, logs, history and reports live under `$STATE_DIRECTORY` (systemd `StateDirectory`), or `~/.local/state/punchcard` otherwise. Everything else, such as the repos, their default branches and the App installations, comes from GitHub.

Without `PUNCHCARD_GITHUB_APP_ID`, Punchcard clones over SSH using your own keys and doesn't post checks. That's handy for trying it without GitHub, by sending it webhooks yourself.

## Deploying

Punchcard ships with a `unit.toml` for deployment via [`unit`](https://github.com/vt100io/unit), which handles the systemd unit, socket activation, and Caddy HTTPS configuration.

```bash
unit deploy
```

## Developing Punchcard

Punchcard builds itself: this repo's `ci` task runs govulncheck, the build and the tests. `mise run serve` builds and runs it with the GitHub App, reading secrets from `~/.config/punchcard/`.

`mise run demo` re-records `demo/demo.gif` with [VHS](https://github.com/charmbracelet/vhs). It runs a throwaway Punchcard with made-up history and local repos, sends it webhooks while recording `punchcard tui`, and touches neither GitHub nor your real state directory.
