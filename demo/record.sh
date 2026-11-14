#!/usr/bin/env bash
# Records demo/demo.gif: VHS types `punchcard tui` against a throwaway
# Punchcard with made-up history and local repos, while demo/scenario.sh
# sends it webhooks. Nothing touches GitHub or your real state directory.
#
#   mise run demo
#
# Needs vhs (with ttyd and ffmpeg), mise, git, openssl and unshare.
set -euo pipefail
cd "$(dirname "$0")/.."

# Short path: the state directory holds a unix socket (~100 byte limit).
root=$(mktemp -d "${XDG_RUNTIME_DIR:-/tmp}/pc-demo.XXXX")
cleanup() {
	kill "$server" 2>/dev/null || true
	rm -rf "$root"
}
trap cleanup EXIT

export DEMO_ROOT=$root DEMO_PORT=18282 DEMO_SECRET=demo
export STATE_DIRECTORY=$root/state

go build -o "$root/punchcard" .
demo/fixtures.sh "$root"

# The server clones git@github.com:vt100io/<repo>.git from the local repos.
env -u PUNCHCARD_GITHUB_APP_ID \
	PORT=$DEMO_PORT PUNCHCARD_SECRET=$DEMO_SECRET \
	GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0="url.$root/origin/.insteadOf" GIT_CONFIG_VALUE_0="git@github.com:" \
	"$root/punchcard" >"$root/server.log" 2>&1 &
server=$!
for _ in $(seq 50); do
	[ -S "$STATE_DIRECTORY/punchcard.sock" ] && break
	sleep 0.1
done

# `punchcard` in the recording: this build, shown on a machine named "ci"
# rather than this one (a private hostname, no root needed).
mkdir -p "$root/bin"
cat >"$root/bin/punchcard" <<-'EOF'
	#!/bin/sh
	exec unshare -Ur -u sh -c 'hostname ci && exec "$0" "$@"' "$DEMO_ROOT/punchcard" "$@"
EOF
chmod +x "$root/bin/punchcard"
export PATH=$root/bin:$PATH

vhs demo/demo.tape
