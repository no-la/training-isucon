#!/usr/bin/env bash
# ローカルの変更をサーバーへ反映
set -euo pipefail

HOST="${ISUCON_HOST:-35.79.218.21}"
USER="${ISUCON_USER:-isucon}"
KEY="${ISUCON_KEY:-$HOME/.ssh/ws-default-keypair.pem}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"

SSH="ssh -i $KEY"
RSYNC_OPTS=(-az -e "$SSH")

# webapp を反映
rsync "${RSYNC_OPTS[@]}" \
  --exclude='node/node_modules' \
  --exclude='python/__pycache__' \
  --exclude='php/vendor' \
  --exclude='ruby/vendor' \
  --exclude='ruby/.bundle' \
  "$REPO/webapp/" "$USER@$HOST:/home/isucon/private_isu/webapp/"

# etc 配下は root 権限が要るのでステージしてから sudo cp
if [[ -d "$REPO/etc" ]]; then
  rsync "${RSYNC_OPTS[@]}" "$REPO/etc/" "$USER@$HOST:/tmp/training-isucon-etc/"
  $SSH "$USER@$HOST" 'sudo rsync -a /tmp/training-isucon-etc/ /etc/ && rm -rf /tmp/training-isucon-etc && sudo systemctl daemon-reload'
fi

echo "push done"
