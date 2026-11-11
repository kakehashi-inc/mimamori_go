# mimamori

[English](README.md)

## 1. システム概要

mimamori は、各ホストで指定した OS のユーザー（Web サーバーや php-fpm の実行ユーザーなど）の権限で行われる操作を auditd とファイルの走査で検知し、セントラルに集めてメールで通知するシステムです。何を監視し、どれを重要（`alert`）とするかは、管理者が監視ルールで定めます。

### 主な機能

- 監視の種類: プログラムの実行、権限で拒否されたファイル操作、指定したディレクトリの読み込み、指定した実行ファイルによる通信の開始、許可したパスの外の書き込み可能なファイルとディレクトリ、許可したパスの中で新しく作られたり変更されたりした実行されるファイル（`.php` など）
- ログインした利用者の操作と cron のジョブを対象外にし、システムから起動されたプロセスだけを監視できます
- 監視ルールは値だけを持ち、版で管理します。組み込みのテンプレート（`base`、`apache`、`nginx`、`php-fpm`、`laravel`、`ffmpeg`）や管理者のテンプレートを組み合わせて作成し、保存した版を指定したエージェントに適用します
- エージェントはトークン（有効期限付き、初期化したエージェントに結び付く、自動更新）で認証し、セントラルが自動で作る独自 CA の証明書を使った HTTPS で接続します
- 検知は種類、OS のユーザー、実行ファイル、対象ごとにまとめ、抑止条件、確認済み、最小間隔と再送の抑制を備えた通知（自社の SMTP サーバー）で扱います
- ブラウザ画面（日本語 / 英語）と、セントラルのホスト上で使う CLI から同じ操作ができます。権限区分は管理者と閲覧者です
- セントラル自身のホストも内蔵エージェントで監視します

### 構成

| 構成要素 | 内容 |
| --- | --- |
| 実行ファイル | `mimamori`（Go の 1 ファイル。セントラル、エージェント、CLI を含む） |
| セントラル | `mimamori central start`。ブラウザ用（HTTP、既定 `:8080`）と、エージェントおよび管理用（独自 CA による HTTPS、既定 `:8443`）の 2 つの待ち受け |
| エージェント | `mimamori agent start`。各ホストで auditd を操作できる権限（`auditctl` の都合で通常は root）で動き、セントラルにだけ接続する |
| データ | SQLite。セントラル管理用の `central.db` と、監視対象ごとの `targets/agent-<id>.db` に分けて保存する |
| 処理の流れ | エージェントは 1 つ、セントラルは基本 2 つ（セントラルの機能と内蔵エージェント）。リクエストを処理するワーカーの最大数は設定で指定（既定 1） |

### 前提

- 対象の OS: Amazon Linux 2023、Rocky Linux 9、AlmaLinux 9、Ubuntu 24.04（x64）
- 各ホスト（セントラルのホストを含む）に auditd が導入され、動いていること
- 通知には自社の SMTP サーバーを使います。外部のサービスやデータベースサーバーは使いません

### 導入の流れ

1. セントラルのホストで設定ファイルを用意して起動します。内蔵エージェントも使う場合は、auditd を操作できる権限（通常は root）で実行します。

    ```bash
    cd <実行ファイルを置いたディレクトリ>
    sudo ./mimamori central example-config | sudo tee central.json >/dev/null
    sudo chmod 600 central.json
    sudo ./mimamori central init-admin --name admin
    sudo ./mimamori central start
    ```

    設定ファイル（`central.json`、`agent.json`）は実行ファイルと同じディレクトリに、データファイルはその `data/` に作られます。`data/` はシンボリックリンクにして別の場所に置けます。

    `server_names` に、エージェントが接続に使うホスト名や IP アドレスを設定してください。プロセスの常駐（systemd など）は環境に合わせて設定します。

2. ブラウザで `http://<セントラル>:8080` を開いてログインし、監視ルールの作成とトークンの発行を行います。
3. 監視対象のホストで、トークンを標準入力から渡してエージェントを初期化し、起動します。

    ```bash
    printf '%s\n' '<トークン>' | sudo mimamori agent init --server https://<セントラル>:8443 --ca-fingerprint <独自CAのフィンガープリント>
    sudo mimamori agent start
    ```

    停止と状態の確認は `sudo mimamori central stop` / `sudo mimamori central status`、エージェントは `sudo mimamori agent stop` / `sudo mimamori agent status` です（supervisor などから常駐させる場合は SIGTERM で終了します）。

4. 必要に応じて、セントラルのホストで CLI を使います。CLI はアカウントと結び付かず最大の権限を持ち、セントラルのデータディレクトリを読める OS ユーザーが使えます。

    ```bash
    sudo ./mimamori ctl agents list
    ```

    認証情報や環境変数は要りません（CLI はセントラルの設定とデータディレクトリを読みます）。実行ファイルは、絶対パスか `./mimamori` のようにパスを付けて起動します。

各ホストで必要な auditd の確認と設定は、画面の「導入の案内」に表示されます。

### ドキュメント

- [Documents/システム仕様.md](Documents/システム仕様.md): 設計（用語、処理の流れ、CLI、設定、監視ルール、通信、auditd の扱い、通知、証明書、画面）
- [Documents/API仕様.md](Documents/API仕様.md): HTTP API
- [Documents/テーブル定義.md](Documents/テーブル定義.md): テーブル定義
- [CHANGELOG.md](CHANGELOG.md): 変更履歴

## 2. 開発者向けリファレンス

### アイコン生成

```bash
convert frontend/public/icons/app-icon-org.png -define icon:auto-resize=256,128,96,64,48,32,24,16 frontend/public/favicon.ico

convert frontend/public/icons/app-icon-org.png -resize 72x72   frontend/public/icons/icon-72x72.png
convert frontend/public/icons/app-icon-org.png -resize 96x96   frontend/public/icons/icon-96x96.png
convert frontend/public/icons/app-icon-org.png -resize 128x128 frontend/public/icons/icon-128x128.png
convert frontend/public/icons/app-icon-org.png -resize 144x144 frontend/public/icons/icon-144x144.png
convert frontend/public/icons/app-icon-org.png -resize 152x152 frontend/public/icons/icon-152x152.png
convert frontend/public/icons/app-icon-org.png -resize 192x192 frontend/public/icons/icon-192x192.png
convert frontend/public/icons/app-icon-org.png -resize 384x384 frontend/public/icons/icon-384x384.png
convert frontend/public/icons/app-icon-org.png -resize 512x512 frontend/public/icons/icon-512x512.png
convert frontend/public/icons/app-icon-org.png -resize 180x180 frontend/public/icons/apple-touch-icon.png
```

### Go 操作コマンド

デバッグモジュールの追加・更新

```bash
go install github.com/go-delve/delve/cmd/dlv@latest
```

モジュールの追加

```bash
go get <package-name>
```

モジュールの追加・ビルド

```bash
go install <package-name>
```

モジュールファイルの作成

```bash
go mod init <module-name>
```

モジュールのダウンロード（モジュール名を省略するとgo.modの全て）

```bash
go mod download <module-name>
```

モジュールの最適化（ソースとgo.modの双方向での一致）

```bash
go mod tidy
```

モジュールの最新化

```bash
go get -u
```

Go バージョンの更新

```bash
go mod tidy --go=1.25
```

キャッシュのクリア

```bash
go clean --cache --testcache
```

### フロントエンド操作コマンド

依存パッケージのインストール

```bash
cd frontend
yarn install
```

ビルド（`frontend/dist`に出力。Goの`go:embed`が参照するため、`go build`や`go vet`の前に必要）

```bash
cd frontend
yarn build
```

型チェック

```bash
cd frontend
yarn lint
```

整形（`src`配下）

```bash
cd frontend
yarn format
```

Material Iconsフォントの配置（`node_modules`から`public/fonts`へコピー）

```bash
cd frontend
yarn setup:fonts
```

### 起動

```bash
go run . service start
```

http://localhost:8080 でWeb画面が開きます（APIサーバーは8081。`service stop` / `status`はAPIポートへ接続します）。実行時データ（DBファイルなど）は実行ファイルと同じ場所の`data/`に作成されます。

### Lintとテスト

```bash
make lint
make test
```

### ビルドやリリース方法

ビルド（事前に`cd frontend && yarn build`が必要）

```bash
go build
```

リリース

```bash
make
```
