#!/usr/bin/env bash
# Exports secrets for `unit` to upload to the server via systemd LoadCredential.
# Run locally at deploy time, never on the server.
#
# Secrets end up at $CREDENTIALS_DIRECTORY/<NAME> in the running service.
# punchcard reads PUNCHCARD_SECRET and GITHUB_APP_PRIVATE_KEY from there.

set -euo pipefail

# The HMAC secret used to verify GitHub webhook signatures.
# Replace with your preferred secret manager, e.g.:
#   export PUNCHCARD_SECRET="$(op read 'op://vault/punchcard/webhook-secret')"
#   export PUNCHCARD_SECRET="$(pass show punchcard/webhook-secret)"
# Defaults to the file written when the GitHub App was created.
export PUNCHCARD_SECRET
PUNCHCARD_SECRET="${PUNCHCARD_SECRET:-$(cat "${PUNCHCARD_SECRET_PATH:-$HOME/.config/punchcard/webhook-secret}")}"

# The GitHub App private key (PEM format) used to authenticate as the app
# and obtain short-lived installation tokens for cloning and posting statuses.
# The app needs: contents:read, statuses:write
export GITHUB_APP_PRIVATE_KEY
GITHUB_APP_PRIVATE_KEY="$(cat "${GITHUB_APP_KEY_PATH:-$HOME/.config/punchcard/github-app.pem}")"
