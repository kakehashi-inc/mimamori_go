# mimamori コマンドリファレンス

[English](CommandReference.md)

```text
mimamori [--data-dir DIR] <command> [arguments] [flags]
```

- `--data-dir DIR`: データディレクトリ。既定は実行ファイルと同じディレクトリの `data/`（実行ファイルの場所が分からない場合はカレントディレクトリの `data/`）。空の値は引数エラー（終了コード 2）です。
- 各コマンドのヘルプは `mimamori <command> --help` で表示します。

## 導入の流れ

セントラルのホストで:

```bash
./mimamori central config set server_names central.example.com
./mimamori central init-admin --name admin
./mimamori central start
```

ブラウザで `http://<セントラル>:9080` を開いてログインし、監視ルールの作成とトークンの発行を行います。

各監視対象のホストで:

```bash
./mimamori agent init --central-host central.example.com
./mimamori agent start
```

## central

セントラルのコマンドです。セントラルのホストで実行します。

| コマンド | 説明 |
| --- | --- |
| `central start` | セントラルを起動する（ブラウザ画面、エージェント用の API、管理用の API、内蔵エージェント）。SIGTERM または SIGINT で終了する |
| `central stop [--timeout DURATION]` | 起動中のセントラルを停止し、停止するまで待つ（既定 120s） |
| `central status` | セントラルが稼働中か、待ち受けとワーカー数を表示する。停止中は終了コード 3 |
| `central init-admin --name NAME [--language auto\|en\|ja]` | 最初の管理者を作成する（管理者が 1 人もいない場合だけ）。パスワードは端末または標準入力の 1 行目から読む |
| `central reset-password --name NAME` | 管理者のパスワードを再設定し、アカウントを有効にして、セッションを無効にする |
| `central config list` | 設定の全項目を、値、設定済みか既定値か、反映のタイミングとともに表示する |
| `central config get NAME` | 1 項目の値を表示する |
| `central config set NAME [VALUE]` | 1 項目を設定する。一覧はカンマ区切り。`smtp.password` は端末または標準入力から読む |
| `central config unset NAME` | 1 項目を既定値に戻す |

`central config` での変更は、動作中のセントラルが次の定期的な確認（30 秒ごと）で反映します。設定画面での変更は保存した時点で反映されます。ただし `web_listen`、`agent_listen`、`workers`、`queue_timeout_sec`、`builtin_agent` はセントラルの再起動後に反映されます。

## agent

エージェントのコマンドです。各監視対象のホストで実行します。

| コマンド | 説明 |
| --- | --- |
| `agent init --central-host HOST [--central-port PORT] [--overwrite] [--ca-fingerprint FP]` | エージェントを初期化する。`HOST` はセントラルのホスト名または IP アドレス、`PORT` の既定は 9081。トークンは端末（エコー無し）または標準入力の 1 行目から読む。`--overwrite` で初期化済みのエージェントを初期化し直す（エージェントの停止中だけ） |
| `agent set-central --host HOST [--port PORT]` | 初期化済みのエージェントの接続先（セントラルのホスト名とポート）を変える。エージェントの停止中だけ使え、新しい接続先に今のトークンで接続できた場合だけ保存する |
| `agent start` | エージェントを起動する。SIGTERM または SIGINT で終了する |
| `agent stop [--timeout DURATION]` | 起動中のエージェントを停止し、その回の処理を終えるまで待つ（既定 300s） |
| `agent status` | エージェントが稼働中か、接続先、トークン、適用中の監視ルール、未送信の報告を表示する。停止中は終了コード 3 |

## ctl

管理用のコマンドです。セントラルの稼働中に、セントラルのホストで実行します。`--json` で JSON のまま表示します。

### agents

| コマンド | 説明 |
| --- | --- |
| `ctl agents list` | エージェントの一覧 |
| `ctl agents show ID` | エージェントと自ホストの情報を表示する |

### tokens

| コマンド | 説明 |
| --- | --- |
| `ctl tokens issue NAME [--valid-days N] [--rule ID] [--rule-version V]` | トークンを発行し、値を表示する |
| `ctl tokens list` | トークンの一覧（値を省略せずに表示） |
| `ctl tokens show ID` | トークンを値を省略せずに表示する |
| `ctl tokens reissue ID [--valid-days N]` | 同じ名前でトークンを再発行する。それまでの値は使えなくなる |
| `ctl tokens unbind ID` | エージェントとの結び付けを解除する |
| `ctl tokens revoke ID` | トークンをすぐに失効させる |

### rules

| コマンド | 説明 |
| --- | --- |
| `ctl rules list` | 監視ルールの一覧 |
| `ctl rules show ID [--version V]` | 監視ルールを表示する（最新または指定の版） |
| `ctl rules create NAME [--template T]... [--choice FIELD=TEMPLATE]... [--file F] [--description D]` | テンプレートから、JSON ファイル（`-` で標準入力）から、または空で監視ルールを作成する |
| `ctl rules export ID [--version V]` | 版の内容を JSON で出力する |
| `ctl rules save ID --file F [--comment C] [--description D]` | 編集した内容を新しい版として保存する |
| `ctl rules edit ID [--comment C] [--editor CMD]` | 最新の版をエディタ（既定 `vi`）で編集し、新しい版として保存する |
| `ctl rules history ID` | 監視ルールの版の一覧 |
| `ctl rules diff ID FROM TO` | 2 つの版の差分を表示する |
| `ctl rules apply ID --agent ID... [--version V]` | 版をエージェントに適用する |
| `ctl rules to-template ID NAME [--version V] [--description D]` | 版から管理者のテンプレートを作る |
| `ctl rules delete ID` | 監視ルールを削除する（適用中は不可） |

### templates

| コマンド | 説明 |
| --- | --- |
| `ctl templates list` | テンプレートの一覧 |
| `ctl templates show NAME` | テンプレートを表示する |
| `ctl templates create NAME [--from-builtin T \| --from-rule ID --version V \| --file F] [--description D]` | 管理者のテンプレートを作成する |
| `ctl templates update NAME --file F [--description D]` | 管理者のテンプレートを変更する |
| `ctl templates delete NAME` | 管理者のテンプレートを削除する |

### detections

| コマンド | 説明 |
| --- | --- |
| `ctl detections list [--agent ID] [--category alert\|normal] [--type T] [--from T] [--to T] [--acknowledged true\|false] [--suppressed true\|false] [--limit N] [--offset N]` | 検知の一覧。`T` は RFC3339 または `YYYY-MM-DD[THH:MM[:SS]]`（ローカル時間）。解釈できない値はエラー |
| `ctl detections show AGENT_ID ID` | 検知を表示する |
| `ctl detections ack AGENT_ID:ID...` | 検知を確認済みにする |

### suppressions

| コマンド | 説明 |
| --- | --- |
| `ctl suppressions list` | 抑止条件の一覧 |
| `ctl suppressions create [--agent ID] [--type T] [--user U] [--exe E] [--target T] [--comment C]` | 抑止条件を作成する（空の項目はすべてに一致し、`--exe` と `--target` は `*` を使える） |
| `ctl suppressions delete ID` | 抑止条件を削除する |

### accounts

| コマンド | 説明 |
| --- | --- |
| `ctl accounts list` | アカウントの一覧 |
| `ctl accounts create NAME --role admin\|viewer [--language auto\|en\|ja]` | アカウントを作成する（パスワードは標準入力から） |
| `ctl accounts role ID admin\|viewer` | 権限区分を変える |
| `ctl accounts password ID` | パスワードを再設定する（標準入力から） |
| `ctl accounts disable ID` | アカウントを無効にする |
| `ctl accounts enable ID` | アカウントを有効にする |
| `ctl accounts delete ID` | アカウントを削除する |

### certs と history

| コマンド | 説明 |
| --- | --- |
| `ctl certs status` | 独自 CA とサーバー証明書の状態を表示する |
| `ctl history [--category C] [--limit N] [--offset N]` | 履歴を表示する。区分: `account`、`rule`、`token`、`certificate`、`rejected`、`template`、`suppression`、`detection`、`config` |

## version

| コマンド | 説明 |
| --- | --- |
| `version` | バージョンを表示する |

## 終了コード

| コード | 意味 |
| --- | --- |
| 0 | 正常 |
| 1 | 一般エラー |
| 2 | 引数エラー（不明なコマンドやフラグ、値の不足など、コマンドラインそのものの誤りを含む） |
| 3 | 設定または DB のエラー（多重起動、待ち受けを開けない場合（使用中など）、エージェントの動作中の `agent init` と `agent set-central` を含む）。`status` では停止中 |
| 4 | 認証または権限のエラー |
| 5 | 実行エラー（外部コマンドまたはセントラル） |
| 6 | ファイル I/O エラー |
| 7 | タイムアウト |
