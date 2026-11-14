#!/usr/bin/env bash
# Sends the demo server signed push webhooks on a timeline, while VHS
# records the TUI: unit builds, molt is queued, then a newer molt push
# replaces the queued one.
set -euo pipefail

push() {
	local repo=$1 n=$2 sha body sig
	sha=$(git -C "$DEMO_ROOT/origin/vt100io/$repo.git" rev-parse "HEAD~$((3 - n))")
	body=$(printf '{"ref":"refs/heads/main","after":"%s","deleted":false,"repository":{"full_name":"vt100io/%s","default_branch":"main"},"sender":{"login":"stianh"}}' "$sha" "$repo")
	sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$DEMO_SECRET" | awk '{print $2}')
	curl -s -o /dev/null -X POST "http://127.0.0.1:$DEMO_PORT/" \
		-H "X-GitHub-Event: push" -H "X-Hub-Signature-256: sha256=$sig" --data "$body"
}

sleep 4
push unit 1
sleep 2
push molt 1
sleep 1.5
push molt 2
