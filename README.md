# training-isucon

日本CTO協会 合同ISUCON研修 2026 (private-isu) の作業ディレクトリ。

## 最終結果 (2026-06-23, コードフリーズ時点)

- **公式ベンチ ベストスコア: 542,489** (15:33 記録、#3 ピーク)
- 初期スコア: 585 (Ruby ベースライン) → **約 927 倍**
- 最終コード: `webapp/golang/app.go` (Go へ移植 + 全 DB in-memory 化 + per-session HTML cache)

### 主な変更

| iter | 内容 | 効果 (公式) |
|---|---|---|
| 1 | comments/posts に index 4 本 | 585 → 42,545 (×72) |
| 2-3 | unicorn workers + 画像 disk 配信 (nginx try_files) | →254,646 |
| 4-5 | digest 高速化, N+1 IN 化, STRAIGHT_JOIN, MySQL/nginx tune | →272,720 |
| 6-8 | imgdata drop + INDEX_CACHE + iter5-7 完成 | →443,957 |
| 9-17 | **Go 実装へ移植**、users/posts/comments/comment_count を全 in-memory 化 | →511,894 |
| 20 | nginx ↔ app を Unix domain socket | (variance内) |
| 22 | GET / の HTML をセッション単位でキャッシュ + cacheVersion bump | **→542,489** |
| 31 | nginx access_log off | (variance内) |
| 33 | log.SetOutput(io.Discard) + Lshortfile 除去 | コードフリーズ時の状態 |

### 失敗したアプローチ (記録)

- iter15: GET /@user の TTL キャッシュ → POST 直後の staleness で公式 237k 大退行
- iter19: session を gorilla CookieStore に → 公式失敗
- iter28-30: shared HTML cache (全セッション共有 + placeholder 置換) 3 種類 → 全敗。GC コスト or singleflight serialization で per-session より遅い
- iter32: post 単位 HTML フラグメントキャッシュ → 公式 264k 退行 (make_posts cache との staleness 連鎖)

詳細は git log を参照。



## セットアップ

```sh
cp .env.local.example .env.local
$EDITOR .env.local  # サーバー IP / SSH 鍵パスを書く
source .env.local
```

`scripts/*.sh` は `ISUCON_HOST` / `ISUCON_USER` / `ISUCON_KEY` を環境変数で読みます。

## アプリ構成 (サーバー側)

- `/home/isucon/private_isu/webapp/` … Webアプリ (Ruby がデフォルト)
- `/home/isucon/private_isu/benchmarker/` … ベンチマーカー
- 起動中: `isu-ruby`, `nginx`, `mysql`, `memcached`
- MySQL: user=`isuconp` password=`isuconp` db=`isuconp`
- memcached: `localhost:11211`
- nginx 設定: `/etc/nginx/sites-enabled/isucon.conf`

## 言語切り替え

```sh
sudo systemctl stop isu-ruby
sudo systemctl disable isu-ruby
sudo systemctl enable isu-go
sudo systemctl start isu-go
```

利用可能: `isu-ruby` / `isu-go` / `php8.3-fpm` / `isu-python` / `isu-node`

## よく使うコマンド

```sh
# SSH
./scripts/ssh.sh

# サーバー側コードをローカルへ同期
./scripts/pull.sh

# ローカルの変更をサーバーへ反映
./scripts/push.sh

# ベンチマーク実行
./scripts/bench.sh

# ベンチ走らせて alp + mysqldumpslow まで一気に取る
./scripts/measure.sh
```

## 計測

- `alp` (ローカル `brew install alp` 済) で nginx LTSV access log を集計
- MySQL slow query log (`long_query_time=0`) を `mysqldumpslow` で集計
- 結果は `measurements/<timestamp>/` に保存 (gitignore)

## 参考

- ワークショップ: https://catalog.us-east-1.prod.workshops.aws/event/dashboard/ja-JP/workshop/performance-tuning/hands-on
- レギュレーション: https://github.com/catatsuy/private-isu/blob/master/public_manual.md
