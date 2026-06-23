#!/usr/bin/env bash
# サーバーにSSH接続する
set -euo pipefail

HOST="${ISUCON_HOST:-35.79.218.21}"
USER="${ISUCON_USER:-isucon}"
KEY="${ISUCON_KEY:-$HOME/.ssh/ws-default-keypair.pem}"

exec ssh -i "$KEY" "$USER@$HOST" "$@"
