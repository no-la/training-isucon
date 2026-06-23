# training-isucon

日本CTO協会 合同ISUCON研修 2026 (private-isu) の作業ディレクトリ。

## サーバー情報

- ServerPublicIP: `35.79.218.21`
- ServerSecurityGroupId: `sg-0988728b9277180f8`
- SSH KeyPair: `~/.ssh/ws-default-keypair.pem`
- ログインユーザー: `isucon`

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
```

## 参考

- ワークショップ: https://catalog.us-east-1.prod.workshops.aws/event/dashboard/ja-JP/workshop/performance-tuning/hands-on
- レギュレーション: https://github.com/catatsuy/private-isu/blob/master/public_manual.md
- 作業ログ: `~/Documents/MyVault/ISUCON研修.md`
