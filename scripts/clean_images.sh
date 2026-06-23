#!/usr/bin/env bash
# bench で追加された投稿画像 (id > 10000) を削除して disk を空ける
set -euo pipefail

HOST="${ISUCON_HOST:-35.79.218.21}"
USER="${ISUCON_USER:-isucon}"
KEY="${ISUCON_KEY:-$HOME/.ssh/ws-default-keypair.pem}"

ssh -i "$KEY" "$USER@$HOST" '
ls /home/isucon/private_isu/webapp/public/image/ \
  | awk -F. "{ if (\$1 > 10000) print \"/home/isucon/private_isu/webapp/public/image/\" \$0 }" \
  | xargs -r rm
echo "cleaned"
df -h / | tail -1
'
