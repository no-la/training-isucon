#!/usr/bin/env bash
# ベンチマークを実行する
set -euo pipefail

HOST="${ISUCON_HOST:-35.79.218.21}"
USER="${ISUCON_USER:-isucon}"
KEY="${ISUCON_KEY:-$HOME/.ssh/ws-default-keypair.pem}"

# ベンチマーカーの引数は workshop の手順に合わせて調整する
exec ssh -i "$KEY" "$USER@$HOST" \
  'cd /home/isucon/private_isu/benchmarker && ./bin/benchmarker -u ./userdata -t http://localhost'
