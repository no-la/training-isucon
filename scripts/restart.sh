#!/usr/bin/env bash
# サービスを再起動 (デフォルト: ruby)
set -euo pipefail

HOST="${ISUCON_HOST:-35.79.218.21}"
USER="${ISUCON_USER:-isucon}"
KEY="${ISUCON_KEY:-$HOME/.ssh/ws-default-keypair.pem}"

LANG_SVC="${1:-isu-ruby}"

ssh -i "$KEY" "$USER@$HOST" \
  "sudo systemctl restart nginx mysql memcached $LANG_SVC && sudo systemctl status --no-pager $LANG_SVC | head -10"
