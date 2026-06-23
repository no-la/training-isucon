#!/usr/bin/env bash
# サーバーのアプリ・設定ファイルをローカルへ同期
set -euo pipefail

HOST="${ISUCON_HOST:-35.79.218.21}"
USER="${ISUCON_USER:-isucon}"
KEY="${ISUCON_KEY:-$HOME/.ssh/ws-default-keypair.pem}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"

SSH="ssh -i $KEY"
RSYNC_OPTS=(-az --delete -e "$SSH")

# webapp (アプリ本体)
rsync "${RSYNC_OPTS[@]}" \
  --exclude='golang/app' \
  --exclude='golang/private_isu_golang' \
  --exclude='python/.venv' \
  --exclude='python/__pycache__' \
  --exclude='node/node_modules' \
  --exclude='php/vendor' \
  --exclude='ruby/vendor' \
  --exclude='ruby/.bundle' \
  "$USER@$HOST:/home/isucon/private_isu/webapp/" "$REPO/webapp/"

# nginx / mysql / sysctl 設定 (要rootで取れる範囲だけ)
mkdir -p "$REPO/etc"
$SSH "$USER@$HOST" 'sudo tar -C / -czf - etc/nginx etc/mysql etc/sysctl.conf etc/sysctl.d 2>/dev/null' \
  | tar -C "$REPO/etc" -xzf - --strip-components=1 2>/dev/null || true

echo "pull done"
