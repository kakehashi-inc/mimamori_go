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
./mimamori central init-admin --email admin@example.com --display-name Admin
./mimamori central start
```

`central start` は SIGTERM または SIGINT を受けるまで前面で動き続けるため、サービス管理の仕組みから実行するか、その端末を開いたままにします。`ctl` のコマンドは別の端末で実行します。

ブラウザで `http://<セントラル>:9080` を開いてログインし、監視ルールの作成とトークンの発行を行います。同じことは `ctl rules create` と `ctl tokens issue` でも行えます。

各監視対象のホストで:

```bash
./mimamori agent init --central-host central.example.com
./mimamori agent start
```

`agent init` はトークン（発行したトークンの値）を尋ねます。`agent start` は SIGTERM または SIGINT を受けるまで前面で動き続けます。

## central

セントラルのコマンドです。セントラルのホストで実行します。

| コマンド | 説明 |
| --- | --- |
| `central start` | セントラルを起動する（ブラウザ画面、エージェント用の API、管理用の API、内蔵エージェント）。SIGTERM または SIGINT で終了する |
| `central stop [--timeout DURATION]` | 起動中のセントラルを停止し、停止するまで待つ（既定 120s） |
| `central status` | セントラルが稼働中か、待ち受けとワーカー数を表示する。稼働中は起動時の値（実際に使っている値）を表示し、保存済みの値が違う場合は `http://0.0.0.0:9080 (saved: 0.0.0.0:9090, takes effect after restart)` のように併記する。停止中は保存済みの値を表示する。停止中は終了コード 3 |
| `central init-admin --email ADDRESS --display-name NAME [--language auto\|en\|ja]` | 最初の管理者を作成する。`ADDRESS` はログインに使うメールアドレス、`NAME` は画面に表示する名前。`--language` の既定は `auto`。管理者が 1 人もいない場合だけ。パスワードは端末または標準入力の 1 行目から読む |
| `central reset-password --email ADDRESS` | このメールアドレス（大文字と小文字は区別しない）の管理者のパスワードを再設定し、アカウントを有効にして、セッション、信頼済みのブラウザ、送信済みの確認コードを無効にする |
| `central config list` | 設定の全項目を、値、設定済みか既定値か、反映のタイミングとともに表示する |
| `central config get NAME` | 1 項目の値を表示する |
| `central config set NAME [VALUE]` | 1 項目を設定する。一覧はカンマ区切り。`smtp.password` は端末または標準入力から読む |
| `central config unset NAME` | 1 項目を既定値に戻す |

`central config` での変更は、動作中のセントラルが次の定期的な確認（30 秒ごと）で反映します。設定画面での変更は保存した時点で反映されます。ただし `web_listen`、`agent_listen`、`workers`、`queue_timeout_sec`、`builtin_agent` はセントラルの再起動後に反映されます。

`smtp.host` を設定していると、ブラウザでのログインには、メールで届く確認コードが要ります。確認コードのメールを送れずにログインできない場合は、`central config set smtp.*` で SMTP の設定を直すか、`central config unset smtp.host` で確認コードを求めない状態にします（どちらもセントラルの停止中でも使える）。アカウントのメールアドレスの誤りは `ctl accounts email ID ADDRESS` で、パスワードは `central reset-password --email ADDRESS` で直します。

## agent

エージェントのコマンドです。各監視対象のホストで実行します。

| コマンド | 説明 |
| --- | --- |
| `agent init --central-host HOST [--central-port PORT] [--overwrite] [--ca-fingerprint FP]` | エージェントを初期化する。`HOST` はセントラルのホスト名または IP アドレス、`PORT` の既定は 9081。トークンは端末（エコー無し）または標準入力の 1 行目から読む。`--overwrite` で初期化済みのエージェントを初期化し直す（エージェントの停止中だけ）。保存していた監視ルールは消え、次に起動した後の最初の同期で、トークンに割り当てられた監視ルールを適用するか、割り当てが無ければエージェントが読み込んだ auditd のルールを削除する |
| `agent set-central --host HOST [--port PORT]` | 初期化済みのエージェントの接続先（セントラルのホスト名とポート）を変える。エージェントの停止中だけ使え、新しい接続先に今のトークンで接続できた場合だけ保存する。`--port` を省略すると今のポートのままにする |
| `agent start` | エージェントを起動する。SIGTERM または SIGINT で終了する |
| `agent stop [--timeout DURATION]` | 起動中のエージェントを停止し、その回の処理を終えるまで待つ（既定 300s） |
| `agent status` | エージェントが稼働中か、接続先、トークン、適用中の監視ルール、未送信の報告を表示する。停止中は終了コード 3 |

## ctl

管理用のコマンドです。セントラルの稼働中に、セントラルのホストで実行します。接続先は `agent_listen` のアドレス（空のホストと `0.0.0.0` の場合は `127.0.0.1`、`::` の場合は `::1`）とポートで、セントラルの証明書をデータディレクトリの `ca.crt` で検証します。管理用の API は、セントラルのホスト自身からの接続だけに応答します。`--json` で JSON のまま表示します（`ctl rules export` と `ctl command-sets export` は常に JSON を出力し、`ctl rules edit` はメッセージだけを表示します）。表では時刻をローカル時間で表示し、それ以外の値はそのまま表示します。長い値は 60 文字に省略します（`ctl tokens list` を除く）。`--file` で指定する内容のファイルは JSON のオブジェクトである必要があります。同時に指定した他のフラグと組み合わせが合わないフラグは、エラーにならずに無視されます。どれが使われるかは、以下の各コマンドの説明に書いています。

### agents

| コマンド | 説明 |
| --- | --- |
| `ctl agents list` | エージェントの一覧 |
| `ctl agents show ID` | エージェントと自ホストの情報を表示する |

### tokens

| コマンド | 説明 |
| --- | --- |
| `ctl tokens issue NAME [--valid-days N] [--rule ID] [--rule-version V]` | トークンを発行する（`--valid-days` の既定はセントラルの設定 `token_valid_days` で、0 以下を指定した場合も同じ。3650 を超える値は引数エラー。`--rule-version` は `--rule` と一緒に指定した場合だけ使われ、既定は最新の版）。値と、監視対象のホストで実行する `agent init` のコマンドを表示する（`agent_listen` のポートが 9081 以外なら `--central-port` を含む） |
| `ctl tokens list` | トークンの一覧（値を省略せずに表示） |
| `ctl tokens show ID` | トークンを値を省略せずに表示する |
| `ctl tokens reissue ID [--valid-days N]` | 同じ名前でトークンを再発行する（`--valid-days` は `tokens issue` と同じ）。それまでの値は使えなくなる。表示する `agent init` のコマンドは `--overwrite` を含む |
| `ctl tokens unbind ID` | エージェントとの結び付けを解除する |
| `ctl tokens revoke ID` | トークンをすぐに失効させる |

### rules

| コマンド | 説明 |
| --- | --- |
| `ctl rules list` | 監視ルールの一覧 |
| `ctl rules show ID [--version V]` | 監視ルールを表示する（指定の版。省略または 0 以下なら最新の版） |
| `ctl rules create NAME [--template T]... [--choice FIELD=TEMPLATE]... [--file F] [--description D]` | テンプレートから、JSON ファイル（`-` で標準入力）から、または空で監視ルールを作成する。`--choice` は 1 つで 1 つの値（`FIELD` はカンマを含んでよい）。`--template` があると `--file` は無視され、`--choice` は `--template` と一緒に指定した場合だけ使われる |
| `ctl rules export ID [--version V]` | 版の内容を JSON で出力する（`--version` を省略すると最新の版） |
| `ctl rules save ID --file F [--comment C] [--description D]` | 編集した内容を新しい版として保存する |
| `ctl rules edit ID [--comment C] [--editor CMD]` | 最新の版をエディタ（既定 `vi`）で編集し、新しい版として保存する |
| `ctl rules history ID` | 監視ルールの版の一覧 |
| `ctl rules diff ID FROM TO` | 2 つの版の差分を表示する |
| `ctl rules apply ID --agent ID... [--version V]` | 版（既定は最新）をエージェントに適用する（同じエージェントを複数回指定しても 1 件として扱う。存在しないエージェントは引数エラー） |
| `ctl rules unassign --agent ID...` | エージェントから監視ルールの割り当てを外す。各エージェントは次の同期で、読み込んだ auditd のルールとルールファイルを削除し、何も適用していないことを報告する（割り当ての無いエージェントはそのままで数えない。同じエージェントを複数回指定しても 1 件として扱う。存在しないエージェントは引数エラー） |
| `ctl rules to-template ID NAME [--version V] [--description D]` | 版（既定は最新）からテンプレートを作る（管理者が作成したテンプレートになる） |
| `ctl rules delete ID` | 監視ルールを削除する（適用中は不可） |

### templates

| コマンド | 説明 |
| --- | --- |
| `ctl templates list` | テンプレートの一覧 |
| `ctl templates show NAME` | テンプレートを表示する |
| `ctl templates create NAME [--from-builtin T \| --from-rule ID [--version V] \| --file F] [--description D]` | テンプレートを作成する（管理者が作成したテンプレートになる。`--from-rule` は `--version` が無ければ最新の版をコピーする。`--version` は `--from-rule` と一緒に指定した場合だけ使われる）。複数の元を指定した場合は、`--from-builtin`、`--from-rule`、`--file` の順で最初のものを使い、他は無視される |
| `ctl templates update NAME --file F [--description D]` | 管理者が作成したテンプレートを変更する（`--description` を省略すると説明はそのまま） |
| `ctl templates delete NAME` | 管理者が作成したテンプレートを削除する |

### detections

| コマンド | 説明 |
| --- | --- |
| `ctl detections list [--agent ID] [--category alert\|normal] [--type T] [--from T] [--to T] [--acknowledged true\|false] [--suppressed true\|false] [--limit N] [--offset N]` | 検知の一覧。`T` はローカル時間の `YYYY-MM-DD HH:MM:SS`（表に表示する形）、`YYYY-MM-DD HH:MM`、`YYYY-MM-DDTHH:MM:SS`、`YYYY-MM-DDTHH:MM`、`YYYY-MM-DD`、または RFC3339。それ以外の値は引数エラー。`--agent` が 0 以下ならすべてのエージェント。`--limit` の既定は 100 で、1 から 500 の範囲に収められる（0 は 1 件、500 を超える値は 500 件）。`--offset` の既定は 0 |
| `ctl detections show AGENT_ID ID` | 検知を表示する |
| `ctl detections ack AGENT_ID:ID...` | 検知を確認済みにする。`AGENT_ID` と `ID` は数字だけで書いた正の整数で、後ろに何も付けない。指定した検知のどれかが存在しない場合は、どれも確認済みにせず、存在しないものを示して終了コード 2 で終わる（他のコマンドで対象が存在しない場合と同じ）。確認済みの検知は存在するものとして扱う |

### suppressions

| コマンド | 説明 |
| --- | --- |
| `ctl suppressions list` | 抑止条件の一覧 |
| `ctl suppressions create [--agent ID] [--type T] [--user U] [--exe E] [--target T] [--comment C]` | 抑止条件を作成する（空の項目はすべてに一致し、`--exe` と `--target` は `*` を使える）。`--type`、`--user`、`--exe`、`--target` のうち少なくとも 1 つが必要で、`--agent` だけの指定は引数エラー（`--agent` を省略するとすべてのエージェントが対象） |
| `ctl suppressions delete ID` | 抑止条件を削除する |

### command-sets

1 つのエージェント（内蔵エージェントを含む）のコマンドセットの定義、実行の指示、結果を扱います。`AGENT_ID` は `ctl agents list` の id です。これらのコマンドで確認コードを求められることはありません。

| コマンド | 説明 |
| --- | --- |
| `ctl command-sets list AGENT_ID` | エージェントのコマンドセットの一覧。表の上に、エージェント、新しいセットの OS のユーザーの既定値（エージェントの実行ユーザー。エージェントが接続する前は `(not reported yet)`）、ホストのタイムゾーン、エージェントが現在の定義を持っているかを表示する |
| `ctl command-sets show AGENT_ID SET_ID` | コマンドセットを表示する。定義、スケジュールの要約、応答の無い実行の指示、最後に終わった実行、実行中か、エージェントが報告したそのセットの問題 |
| `ctl command-sets export AGENT_ID SET_ID` | コマンドセットの定義だけを、`create` と `update` が読む形の JSON で出力する（常に JSON）。日時を指定した 1 回の日時は、ローカル時間のオフセット付きで出力する |
| `ctl command-sets create AGENT_ID --file F` | 定義ファイル（`-` で標準入力）からコマンドセットを作成する。`os_user` を省略すると、エージェントの実行ユーザーを使う（一度も接続していないエージェントには無いので、`os_user` が必要） |
| `ctl command-sets update AGENT_ID SET_ID --file F` | コマンドセットの定義全体を定義ファイルの内容に置き換える。`os_user` を省略すると、そのセットの現在の OS のユーザーのまま |
| `ctl command-sets delete AGENT_ID SET_ID` | コマンドセットを削除する。実行の結果は保存期間の処理で消えるまで残る。まだ配信していない実行の指示は破棄する |
| `ctl command-sets import AGENT_ID --from SOURCE_AGENT_ID SET_ID[=NEW_NAME]...` | エージェント `SOURCE_AGENT_ID`（`AGENT_ID` 自身でもよい）のコマンドセットを、OS のユーザーと作業ディレクトリを含めてすべて `AGENT_ID` に写す。`=NEW_NAME` で写したセットの名前を変える（既定は元の名前。最初の `=` より後がすべて名前）。名前が写し先ですでに使われているか、インポートの中で重なる場合は、何も作らずにその名前を表示する。`SET_ID` は正の整数で、`NEW_NAME` は空にできない |
| `ctl command-sets run AGENT_ID SET_ID` | コマンドセットの実行を指示する（定期のセットにも出せる）。エージェントは次の同期で指示を受け取る。1 つのセットに、エージェントが応答していない指示は 1 つだけ。24 時間以内に配信されなかった指示は期限切れになる |
| `ctl command-sets discard AGENT_ID REQUEST_ID` | まだエージェントに配信していない実行の指示を破棄する（`REQUEST_ID` は `run` が表示した `id`、または `show` の `request.id`） |
| `ctl command-sets runs AGENT_ID SET_ID [--limit N] [--before RUN_ID]` | コマンドセットの実行の一覧を新しい順に表示する（削除したセットも、実行が残っている間は表示できる）。きっかけ、定期の予定の日時、開始、終了、状態、理由、1 行にした表示用の値。`--limit` の既定は 50 で、1 から 500 の範囲に収められる。より古い実行があれば、次のページの `--before` の値を表の下に表示する |
| `ctl command-sets show-run AGENT_ID RUN_ID` | 実行を表示する。きっかけ、OS のユーザー、作業ディレクトリ、日時、状態、表示用の値と、実行したコマンドごとの結果、終了コード、日時、コマンドライン、備考、コンソール出力（1 コマンド 1 MiB まで）、実行しなかったコマンドの数 |

定義ファイルは、API の `CommandSetSpec` の形の JSON のオブジェクト 1 つです（項目、値、上限は [API仕様.md](API仕様.md) の 4.1 章）。未知の項目は拒否されます。省略した項目は既定値になります（実行方法はスケジュールの有無から、保存する実行は 100 回、つなぎ方はカンマ、成功の戻り値は 0、失敗の戻り値は 1 以上、`on_problems` は `stop`、300 秒、20 行）。例:

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

- `work_dir` が空なら OS のユーザーのホームディレクトリです。どのコマンドラインも、セットの OS のユーザーとして、書いたとおりに bash で実行されます。
- 繰り返しのスケジュール（`weekly`、`daily`、`every_hours`、`every_minutes`、`hourly_at`）は、監視対象のホストのローカル時間で実行されます。日時を指定した 1 回の日時（`{"kind": "once", "at": "2026-12-01 03:00"}`）はオフセットを省略でき（`YYYY-MM-DD HH:MM[:SS]`。空白の代わりに `T` も可）、その場合は `ctl` を実行したホストのローカル時間として解釈します。過去の日時は拒否されます。
- セントラルが拒否した値（不正な定義、エージェントの別のセットと同じ名前、応答の無い実行の指示がある状態での 2 つ目の指示）は、終了コード 2 で、項目と、コマンドならその位置（1 から）を示すメッセージを表示します。

### accounts

| コマンド | 説明 |
| --- | --- |
| `ctl accounts list` | アカウントの一覧（id、email、display_name、role、language、disabled、notify、locked_until、created_at） |
| `ctl accounts create EMAIL --display-name NAME --role admin\|viewer [--language auto\|en\|ja]` | アカウントを作成する。`EMAIL` はログインに使うメールアドレス（一意。大文字と小文字は区別しない）、`NAME` は画面に表示する名前（重複してもよい）。`--language` の既定は `auto`。パスワードは標準入力から |
| `ctl accounts email ID ADDRESS` | メールアドレスを確認コードなしで即時に変える。そのアカウントのセッション、信頼済みのブラウザ、送信済みの確認コードは無効になる |
| `ctl accounts display-name ID NAME` | 表示名を変える（セッションはそのまま） |
| `ctl accounts role ID admin\|viewer` | 権限を変える（そのアカウントのセッションは無効になる） |
| `ctl accounts notify ID on\|off` | そのアカウントが通知メール（検知の通知）を受け取るかどうかを選ぶ。作成時は `off`。無効にしたアカウントは選択を保ったまま、無効の間は受け取らない。セッションはそのまま |
| `ctl accounts password ID` | パスワードを再設定する（標準入力から）。そのアカウントのセッション、信頼済みのブラウザ、送信済みの確認コードは無効になる |
| `ctl accounts disable ID` | アカウントを無効にする。そのアカウントのセッション、信頼済みのブラウザ、送信済みの確認コードは無効になる |
| `ctl accounts enable ID` | アカウントを有効にする |
| `ctl accounts delete ID` | アカウントを削除する |

### certs と history

| コマンド | 説明 |
| --- | --- |
| `ctl certs status` | 独自 CA とサーバー証明書の状態を表示する |
| `ctl history [--category C] [--limit N] [--offset N]` | 履歴を表示する（`--limit` の既定は 50 で、1 から 500 の範囲に収められる。`--offset` の既定は 0）。分類: `account`、`rule`、`token`、`certificate`、`rejected`、`template`、`suppression`、`detection`、`config`、`command` |

## version

| コマンド | 説明 |
| --- | --- |
| `version` | バージョンを表示する |

## 終了コード

| コード | 意味 |
| --- | --- |
| 0 | 正常 |
| 1 | 一般エラー |
| 2 | 引数エラー（不明なコマンドやフラグ、値の不足など、コマンドラインそのものの誤りと、名前の重複や組み込みテンプレートの変更など、セントラルが受け付けない値を含む） |
| 3 | 設定または DB のエラー（多重起動、待ち受けを開けない場合（使用中など）、エージェントの動作中の `agent init` と `agent set-central`、別のビルドのプログラムが作ったデータディレクトリの DB を含む。そのような DB を開くコマンドは `status` も含めてファイル名を示すメッセージで失敗し、データディレクトリを作り直す必要がある）。`status` では停止中 |
| 4 | 認証または権限のエラー（`ctl` では、`cli.secret` が稼働中のセントラルと一致しない場合と、セントラルの証明書を `ca.crt` で検証できない場合） |
| 5 | 実行エラー（外部コマンドまたはセントラル） |
| 6 | ファイルまたはディレクトリの I/O エラー（`ctl` では、`cli.secret` や `ca.crt` を読めない場合と、`ca.crt` に証明書が無い場合を含む） |
| 7 | タイムアウト |
