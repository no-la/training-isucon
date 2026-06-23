# training-isucon

日本CTO協会 合同ISUCON研修 2026 (private-isu) の作業ディレクトリ。

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
