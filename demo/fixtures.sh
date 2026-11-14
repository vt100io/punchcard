#!/usr/bin/env bash
# Creates the demo's made-up world in $1: a build history with logs, and
# small Go repos (real go vet and go test, slowed down to watchable speed)
# that the demo server clones as if they were on GitHub.
set -euo pipefail
root=$1
state=$root/state
mkdir -p "$state/logs"

# --- repos ---------------------------------------------------------------

# repo NAME PACKAGE...: a Go module whose tests take ~0.7s per package.
repo() {
	local name=$1 dir=$root/origin/vt100io/$1.git
	shift
	mkdir -p "$dir"
	cat >"$dir/go.mod" <<-EOF
		module github.com/vt100io/$name

		go 1.27
	EOF
	printf 'package main\n\nfunc main() {}\n' >"$dir/main.go"
	for pkg in "$@"; do
		local p=$dir/$pkg base
		base=$(basename "$pkg")
		mkdir -p "$p"
		printf 'package %s\n' "$base" >"$p/$base.go"
		cat >"$p/${base}_test.go" <<-EOF
			package $base

			import (
				"testing"
				"time"
			)

			func TestIt(t *testing.T) { time.Sleep(700 * time.Millisecond) }
		EOF
	done
	cat >"$dir/mise.toml" <<-'EOF'
		[tools]
		go = "1.27.4"

		[tasks.vet]
		run = "go vet ./..."

		[tasks.test]
		run = "go test -count=1 -p 1 ./..."

		[tasks.ci]
		depends = ["vet", "test"]
		run = "go build -o bin/app ."
	EOF
	git -C "$dir" init -q -b main
	git -C "$dir" add .
	git -C "$dir" -c user.name=demo -c user.email=demo@example.com commit -q -m "Initial commit"
	# Two more commits, so a push can supersede a queued one.
	for n in 2 3; do
		git -C "$dir" -c user.name=demo -c user.email=demo@example.com commit -q --allow-empty -m "Change $n"
	done
}

repo unit internal/deploy internal/sshutil internal/systemd internal/audit internal/config cmd/inspector
repo molt internal/build internal/compile internal/duckdb internal/project
repo punchcard history feed control
repo smed dag facts module secret

# --- history ---------------------------------------------------------------

ago() { date -Iseconds -d "-$1"; }
hex() { head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
sha() { head -c 20 /dev/urandom | od -An -tx1 | tr -d ' \n'; }

# build REPO RESULT STARTED SECONDS: a history line and its log.
build() {
	local repo=$1 result=$2 started=$3 secs=$4 id sha finished
	id=$(hex)
	sha=$(sha)
	finished=$(date -Iseconds -d "@$(($(date -d "$started" +%s) + secs))")
	printf '{"id":"%s","repo":"vt100io/%s","event":"push","branch":"main","sha":"%s","who":"stianh","task":"ci","result":"%s","started":"%s","finished":"%s"}\n' \
		"$id" "$repo" "$sha" "$result" "$started" "$finished" >>"$state/history.jsonl"
	{
		printf 'repo:    vt100io/%s\nevent:   push\nbranch:  main\ncommit:  %s\ntask:    ci\nstarted: %s\n\n' "$repo" "$sha" "$started"
		cat
	} >"$state/logs/$id.log"
}

# A repo without a ci task: skipped, no check posted.
build dotfiles no-task "$(ago '4 days')" 1 <<-'EOF'
	no "ci" task in mise config

	SKIPPED after 0.412s
EOF

build unit passed "$(ago '1 day')" 8 <<-'EOF'
	[vet] $ go vet ./...
	[test] $ go test -count=1 -p 1 ./...
	[vet] Finished in 1.21s
	[test] ok  	github.com/vt100io/unit/internal/deploy	0.712s
	[test] Finished in 4.33s
	[ci] $ go build -o bin/app .
	[ci] Finished in 0.41s
	Finished in 4.81s

	PASSED after 7.902s
EOF

build molt passed "$(ago '3 hours')" 6 <<-'EOF'
	[vet] $ go vet ./...
	[test] $ go test -count=1 -p 1 ./...
	[test] ok  	github.com/vt100io/molt/internal/build	0.711s
	[test] Finished in 3.12s
	[ci] $ go build -o bin/app .
	Finished in 3.70s

	PASSED after 5.884s
EOF

build smed failed "$(ago '2 hours')" 7 <<-'EOF'
	[vet] $ go vet ./...
	[test] $ go test -count=1 -p 1 ./...
	[vet] Finished in 1.08s
	[test] ok  	github.com/vt100io/smed/dag	0.714s
	[test] ok  	github.com/vt100io/smed/facts	0.709s
	[test] --- FAIL: TestRollingRestart (0.70s)
	[test]     rolling_test.go:88: restarted web-2 before web-1 reported healthy
	[test]     rolling_test.go:89: want order [web-1 web-2], got [web-2 web-1]
	[test] FAIL
	[test] FAIL	github.com/vt100io/smed/module	0.713s
	[test] ok  	github.com/vt100io/smed/secret	0.708s
	[test] FAIL
	[test] ERROR task failed
	Finished in 3.62s

	FAILED (exit status 1) after 6.84s
EOF

build punchcard passed "$(ago '25 minutes')" 6 <<-'EOF'
	[vet] $ go vet ./...
	[test] $ go test -count=1 -p 1 ./...
	[test] ok  	github.com/vt100io/punchcard/history	0.705s
	[test] Finished in 2.41s
	[ci] $ go build -o bin/app .
	Finished in 2.93s

	PASSED after 5.511s
EOF
