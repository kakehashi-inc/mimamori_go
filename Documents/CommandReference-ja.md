# mimamori コマンドリファレンス

[English](CommandReference.md)

```text
mimamori [--data-dir DIR] <command> [arguments] [flags]
```

- `--data-dir DIR`: データディレクトリ。既定は実行ファイルと同じディレクトリの `data/`。
- `mimamori <command> --help` で各コマンドのヘルプを表示します。

## 導入の流れ

セントラルのホストで:

```bash
./mimamori central config set server_names central.example.com
./mimamori central init-admin --email admin@example.com --display-name Admin
./mimamori central start
```

ブラウザで `https://<セントラル>:9580` を開いてログインし、監視ルールの作成とトークンの発行を行います。

各監視対象のホストで:

```bash
./mimamori agent init --central-host central.example.com
./mimamori agent start
```

`agent init` はトークンの値を尋ねます。`central start` と `agent start` は、SIGTERM または SIGINT を受けるまで前面で動き続けます。

## central

セントラルのコマンドです。セントラルのホストで実行します。

| コマンド | 説明 |
| --- | --- |
| `central start` | セントラルを起動する |
| `central stop [--timeout DURATION]` | 起動中のセントラルを停止し、停止するまで待つ。`--timeout` の既定は 120s |
| `central status` | セントラルの稼働状況、データディレクトリ、待ち受けのアドレスを表示する |
| `central init-admin --email ADDRESS --display-name NAME [--language auto\|en\|ja]` | 最初の管理者を作成する。パスワードは端末または標準入力から読む。`--language` の既定は `auto` |
| `central reset-password --email ADDRESS` | 管理者のパスワードを再設定する。パスワードは端末または標準入力から読む |
| `central config list` | 設定の全項目を表示する |
| `central config get NAME` | 1 項目の値を表示する |
| `central config set NAME [VALUE]` | 1 項目を設定する。一覧はカンマ区切り。`smtp.password` は端末または標準入力から読む |
| `central config unset NAME` | 1 項目を既定値に戻す |

## agent

エージェントのコマンドです。各監視対象のホストで実行します。

| コマンド | 説明 |
| --- | --- |
| `agent init --central-host HOST [--central-port PORT] [--overwrite] [--ca-fingerprint FP]` | エージェントを初期化する。トークンは端末または標準入力から読む。`--central-port` の既定は 9581。`--overwrite` で初期化済みのエージェントを初期化し直す。`--ca-fingerprint` でセントラルの CA のフィンガープリントを確かめる |
| `agent set-central --host HOST [--port PORT]` | 接続先のセントラルを変える。`--port` を省略すると今のポートのまま |
| `agent set-workers N [--builtin]` | エージェントのワーカー数（1〜64、既定 1）を設定する。`--builtin` を付けると、セントラルのホストで内蔵エージェントの数を設定する。次の起動で反映する |
| `agent start` | エージェントを起動する |
| `agent stop [--timeout DURATION]` | 起動中のエージェントを停止し、停止するまで待つ。`--timeout` の既定は 300s |
| `agent status` | エージェントの稼働状況、接続先、トークン、ワーカー数、適用中の監視ルール、未送信の報告を表示する |

## ctl

管理用のコマンドです。セントラルの稼働中に、セントラルのホストで、データディレクトリを読めるユーザーが実行します。`--json` を付けると JSON で出力します。

### agents

| コマンド | 説明 |
| --- | --- |
| `ctl agents list` | エージェントの一覧 |
| `ctl agents show ID` | エージェントと自ホストの情報を表示する |

### tokens

| コマンド | 説明 |
| --- | --- |
| `ctl tokens issue NAME [--valid-days N] [--rule ID] [--rule-version V]` | トークンを発行し、監視対象のホストで実行する `agent init` のコマンドを表示する。`--valid-days` の既定は設定の `token_valid_days`。`--rule-version` の既定は最新の版 |
| `ctl tokens list` | トークンの一覧 |
| `ctl tokens show ID` | トークンを表示する |
| `ctl tokens reissue ID [--valid-days N]` | 同じ名前でトークンを再発行する。それまでの値は使えなくなる |
| `ctl tokens unbind ID` | エージェントとの結び付けを解除する |
| `ctl tokens revoke ID` | トークンを失効させる |

### rules

| コマンド | 説明 |
| --- | --- |
| `ctl rules list` | 監視ルールの一覧 |
| `ctl rules show ID [--version V]` | 監視ルールを表示する。`--version` の既定は最新の版 |
| `ctl rules create NAME [--template T]... [--choice FIELD=TEMPLATE]... [--file F] [--description D]` | テンプレート、JSON ファイル（`-` で標準入力）、または空から監視ルールを作成する |
| `ctl rules export ID [--version V]` | 版の内容を JSON で出力する |
| `ctl rules save ID --file F [--comment C] [--description D]` | ファイルの内容を新しい版として保存する |
| `ctl rules edit ID [--comment C] [--editor CMD]` | 最新の版をエディタ（既定 `vi`）で編集し、新しい版として保存する |
| `ctl rules history ID` | 版の一覧 |
| `ctl rules diff ID FROM TO` | 2 つの版の差分を表示する |
| `ctl rules apply ID --agent ID... [--version V]` | 版をエージェントに適用する。`--version` の既定は最新の版 |
| `ctl rules unassign --agent ID...` | エージェントから監視ルールの割り当てを外す |
| `ctl rules to-template ID NAME [--version V] [--description D]` | 版からテンプレートを作成する |
| `ctl rules delete ID` | 監視ルールを削除する |

### templates

| コマンド | 説明 |
| --- | --- |
| `ctl templates list` | テンプレートの一覧 |
| `ctl templates show NAME` | テンプレートを表示する |
| `ctl templates create NAME [--from-builtin T \| --from-rule ID [--version V] \| --file F] [--description D]` | 組み込みのテンプレート、監視ルールの版、またはファイルからテンプレートを作成する |
| `ctl templates update NAME --file F [--description D]` | 管理者が作成したテンプレートを変更する |
| `ctl templates delete NAME` | 管理者が作成したテンプレートを削除する |

### detections

| コマンド | 説明 |
| --- | --- |
| `ctl detections list [--agent ID] [--category alert\|normal] [--type exec\|denied\|read\|network\|writable\|exec_file] [--from T] [--to T] [--acknowledged true\|false] [--suppressed true\|false] [--limit N] [--offset N]` | 検知の一覧。`T` はローカル時間の `YYYY-MM-DD HH:MM:SS`、`YYYY-MM-DD` など、または RFC3339。`--limit` の既定は 100（最大 500） |
| `ctl detections show AGENT_ID ID` | 検知を表示する |
| `ctl detections ack AGENT_ID:ID...` | 検知を確認済みにする |

### suppressions

| コマンド | 説明 |
| --- | --- |
| `ctl suppressions list` | 抑止条件の一覧 |
| `ctl suppressions create [--agent ID] [--type T] [--user U] [--exe E] [--target T] [--comment C]` | 抑止条件を作成する。`--agent` を省略するとすべてのエージェントが対象。`--exe` と `--target` には `*` を使える |
| `ctl suppressions delete ID` | 抑止条件を削除する |

### command-sets

`AGENT_ID` は `ctl agents list` の id です。

| コマンド | 説明 |
| --- | --- |
| `ctl command-sets list AGENT_ID` | エージェントのコマンドセットの一覧 |
| `ctl command-sets show AGENT_ID SET_ID` | コマンドセットを表示する |
| `ctl command-sets export AGENT_ID SET_ID` | コマンドセットの定義を JSON で出力する |
| `ctl command-sets create AGENT_ID --file F` | 定義ファイル（`-` で標準入力）からコマンドセットを作成する |
| `ctl command-sets update AGENT_ID SET_ID --file F` | コマンドセットの定義を定義ファイルの内容に置き換える |
| `ctl command-sets delete AGENT_ID SET_ID` | コマンドセットを削除する |
| `ctl command-sets import AGENT_ID --from SOURCE_AGENT_ID SET_ID[=NEW_NAME]...` | 別のエージェントのコマンドセットを写す。`=NEW_NAME` で名前を変える |
| `ctl command-sets run AGENT_ID SET_ID` | コマンドセットの実行を指示する |
| `ctl command-sets discard AGENT_ID REQUEST_ID` | エージェントに渡す前の実行の指示を破棄する |
| `ctl command-sets runs AGENT_ID SET_ID [--limit N] [--before RUN_ID]` | 実行の一覧を新しい順に表示する。`--limit` の既定は 50 |
| `ctl command-sets show-run AGENT_ID RUN_ID` | 実行の結果を表示する |

定義ファイルは JSON のオブジェクト 1 つです（項目は [API仕様.md](API仕様.md) の 4.1 章）。例:

```json
{
  "name": "disk usage",
  "description": "Usage of the root file system",
  "purpose": "collect_output",
  "work_dir": "",
  "schedule": {"kind": "daily", "time": "03:00"},
  "commands": [
    {"command_line": "df -h /", "patterns": ["(?P<use>\\d+%)"]}
  ]
}
```

### accounts

| コマンド | 説明 |
| --- | --- |
| `ctl accounts list` | アカウントの一覧 |
| `ctl accounts create EMAIL --display-name NAME --role admin\|viewer [--language auto\|en\|ja]` | アカウントを作成する。パスワードは標準入力から読む。`--language` の既定は `auto` |
| `ctl accounts email ID ADDRESS` | メールアドレスを変える |
| `ctl accounts display-name ID NAME` | 表示名を変える |
| `ctl accounts role ID admin\|viewer` | 権限を変える |
| `ctl accounts notify ID on\|off` | 通知メールを受け取るかどうかを設定する |
| `ctl accounts password ID` | パスワードを再設定する。パスワードは標準入力から読む |
| `ctl accounts disable ID` | アカウントを無効にする |
| `ctl accounts enable ID` | アカウントを有効にする |
| `ctl accounts delete ID` | アカウントを削除する |

### certs と history

| コマンド | 説明 |
| --- | --- |
| `ctl certs status` | 独自 CA とサーバー証明書の状態を表示する |
| `ctl history [--category C] [--limit N] [--offset N]` | 履歴を表示する。`C` は `account`、`rule`、`token`、`certificate`、`rejected`、`template`、`suppression`、`detection`、`config`、`command` のいずれか。`--limit` の既定は 50 |

## version

| コマンド | 説明 |
| --- | --- |
| `version` | バージョンを表示する |

## 終了コード

| コード | 意味 |
| --- | --- |
| 0 | 正常 |
| 1 | 一般エラー |
| 2 | 引数エラー |
| 3 | 設定または DB のエラー。`status` では停止中 |
| 4 | 認証または権限のエラー |
| 5 | 実行エラー（外部コマンドまたはセントラル） |
| 6 | ファイルまたはディレクトリの読み書きのエラー |
| 7 | タイムアウト（`stop` が `--timeout` の時間を過ぎた） |
