# mimamori

[English](README.md)

## 1. システム概要

mimamori は、各ホストで指定した OS のユーザーの権限で行われる操作を auditd とファイルの走査で検知し、セントラルに集めてメールで通知するシステムです。何を監視し、どれを重要とするかは、管理者が監視ルールで定めます。

### 主な機能

- プログラムの実行、権限で拒否されたファイル操作、指定したディレクトリの読み込み、指定した実行ファイルによる通信、書き込み可能なファイルや新しく作られた実行されるファイルの検知
- テンプレートを組み合わせた監視ルールの作成と、版の管理、エージェントへの適用
- 検知の確認、抑止条件、メールでの通知
- ブラウザ画面（日本語 / 英語）とセントラルのホスト上の CLI での管理
- セントラル自身のホストの監視（内蔵エージェント）

### 前提

- 対象の OS: Amazon Linux 2023、Rocky Linux 9、AlmaLinux 9、Ubuntu 24.04（x64）
- 各ホスト（セントラルのホストを含む）で auditd が動いていること
- 通知に使う SMTP サーバー

### 起動と停止

各コマンドの使い方は [コマンドリファレンス](Documents/CommandReference-ja.md) を参照してください。

セントラルの起動と停止:

```bash
./mimamori central start
./mimamori central stop
```

エージェントの起動と停止:

```bash
./mimamori agent start
./mimamori agent stop
```

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
go run . --data-dir data central start
```

http://localhost:9080 でWeb画面が開きます（エージェント用は9081）。実行時データ（DBファイルなど）はカレントディレクトリの`data/`に作成されます。

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
