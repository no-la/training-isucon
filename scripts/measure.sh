#!/usr/bin/env bash
# ログをローテートしてベンチを走らせ、結果と集計を出す
set -euo pipefail

HOST="${ISUCON_HOST:-35.79.218.21}"
USER="${ISUCON_USER:-isucon}"
KEY="${ISUCON_KEY:-$HOME/.ssh/ws-default-keypair.pem}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
TS=$(date +%Y%m%d-%H%M%S)
OUT="$REPO/measurements/$TS"
mkdir -p "$OUT"

SSH="ssh -i $KEY $USER@$HOST"

echo "== rotate logs & enable slow log =="
$SSH "sudo truncate -s 0 /var/log/nginx/access.log /var/log/mysql/mysql-slow.log && mysql -u isuconp -pisuconp -e 'SET GLOBAL slow_query_log = ON;'"

trap '$SSH "mysql -u isuconp -pisuconp -e \"SET GLOBAL slow_query_log = OFF;\"" >/dev/null 2>&1 || true' EXIT

echo "== run benchmark =="
$SSH '/home/isucon/private_isu/benchmarker/bin/benchmarker -u /home/isucon/private_isu/benchmarker/userdata -t http://localhost' \
  | tee "$OUT/bench.json"

$SSH "mysql -u isuconp -pisuconp -e 'SET GLOBAL slow_query_log = OFF;'" >/dev/null 2>&1 || true

echo "== fetch logs =="
$SSH 'sudo cat /var/log/nginx/access.log' > "$OUT/access.log"
$SSH 'sudo cat /var/log/mysql/mysql-slow.log' > "$OUT/mysql-slow.log"

echo "== alp report (top 30 by sum reqtime) =="
alp ltsv --file "$OUT/access.log" \
  --sort sum --reverse \
  --matching-groups "/posts/\d+,/@[^/]+,/image/\d+\.(jpg|png|gif)" \
  -o count,method,uri,min,avg,p99,sum \
  | tee "$OUT/alp.txt" | head -40

echo "== mysqldumpslow (top by total time) =="
$SSH 'sudo mysqldumpslow -s t /var/log/mysql/mysql-slow.log | head -50' \
  | tee "$OUT/mysql-slow-digest.txt" | head -40

echo "== saved to $OUT =="
