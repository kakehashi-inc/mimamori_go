# API 仕様 - mimamori

セントラルが提供する HTTP API の仕様です。ブラウザ画面（`frontend/`）、CLI、エージェントはすべて本書の API を使います。

## 1. 共通

### 1.1 待ち受けとパス

| 待ち受け | 通信 | パス | 利用者 | 認証 |
| --- | --- | --- | --- | --- |
| ブラウザ用（既定 `0.0.0.0:9080`） | HTTP | `/api/v1/*` | ブラウザ画面 | ログインセッション（Cookie `mimamori_session`） |
| ブラウザ用 | HTTP | `/*` | ブラウザ | なし（埋め込み SPA） |
| エージェントおよび管理用（既定 `0.0.0.0:9081`） | HTTPS（独自CA） | `/agent/v1/*` | エージェント | トークン（`Authorization: Bearer mmr_...`）とインスタンスID |
| エージェントおよび管理用 | HTTPS（独自CA） | `/admin/v1/*` | CLI | 接続元がループバックで、CLI 用の秘密（セントラルがデータディレクトリの `cli.secret` に書く値。`Authorization: Bearer <値>`） |

- `/api/v1/X` と `/admin/v1/X` は、認証の方法を除いて同じ操作です（以下ではパスを `/api/v1` を省略して記載します）。`/admin/v1` は CLI 用で、アカウントとは結び付かず最大の権限を持ちます。
- 例外として、2 章の `/health`、`/auth/login`、`/auth/login/verify`、`/auth/logout`、`/auth/password`、`/auth/language`、`/auth/display-name`、`/auth/email`、`/auth/email/verify`、`/auth/step-up`、`/auth/step-up/verify`、`/ui/modules` は `/api/v1` だけにあり、`/auth/me` は両方にあります。
- ブラウザ用で認証の要らない（公開の）パスは `GET /health`、`POST /auth/login`、`POST /auth/login/verify` だけです。
- ループバック以外からの `/admin/v1/*` は 404 を返します。接続元の判定には TCP の接続元アドレスだけを使います。
- 1 接続で 1 リクエストを処理し、応答は `Connection: close` です。

### 1.2 形式

- 要求・応答は JSON（`Content-Type: application/json`）。ブラウザ用と管理用の API では、日時は RFC3339（UTC）の文字列、未設定は `null` です。型で `string` と宣言した日時は、通常は `null` になりません（`null` になり得る項目は型に `| null` を付けています）。エージェント用の API（14 章）では、日時は Unix 秒（整数）です。
- 要求の本文は JSON の値 1 つだけです。その後ろに空白以外のデータが続く場合は 400 を返します。
- ブラウザ用と管理用の API は、要求の JSON に未知のフィールドがあると 400 を返します。エージェント用の API は未知のフィールドを無視します。
- エラー時は 4xx/5xx と `{"error": "<message>"}` を返します。
  - 400: 入力の誤り、401: 未認証、403: CSRF のヘッダーが無い、または組み込みのテンプレートの変更、404: 存在しない、409: 状態の衝突（重複、適用中の監視ルールの削除など）、410: 確認コードの期限切れなど（2 章）、423: ログインの一時停止、428: 再認証が必要（コマンドセットの変更。2 章）、500: 内部エラー（`internal error`）、503: 混雑、または確認コードのメールを送れない（2 章）。
  - 503 を返すのはブラウザ用の待ち受けだけです。エージェントおよび管理用の待ち受け（HTTPS）では、混雑すると応答を返さずに接続を切ります。
- 存在しないパスと、そのパスで対応していないメソッドへの要求は、`{"error": ...}` ではなく `text/plain` の `404 page not found` と `405 Method Not Allowed` を返します（ブラウザ用はログイン済みの場合）。
- ブラウザ用の待ち受けで GET 以外を送る場合は、CSRF 対策としてヘッダー `X-Mimamori-Request: 1` を必須とします（無い場合は 403）。
- ブラウザ画面が利用者の操作によらずに送る要求（開いている画面の定期的な更新）には、ヘッダー `X-Mimamori-Background: 1` を付けます。この要求も認証しますが、ログインセッションの期限を延ばしません（[システム仕様.md](システム仕様.md) のログインセッション）。

### 1.3 権限

`A` は管理者のみ、`V` は管理者と閲覧者が使えます。権限の判定はすべてのリクエストでセントラルが行います。

ブラウザ用（`/api/v1`）では、`A` のルートは閲覧者には存在しません。閲覧者が `A` のルートに要求すると、どのメソッドでも、存在しないルートと同じ応答（状態、ヘッダー、本文）を返します。つまり、そのパスに閲覧者が使えるルートが無ければ `404 page not found`、あれば（例: `POST /tokens` に対する `GET /tokens`）`405 Method Not Allowed` と、閲覧者が使えるメソッドだけを示す `Allow` ヘッダーです。未ログインの要求は 401、CSRF のヘッダーが無い GET 以外の要求は 403 です（いずれもルートの有無に関係ない）。`/admin/v1` は常に最大の権限なので影響しません。

操作者を表す項目（`HistoryItem.account_name`、`created_by`、`acknowledged_by`）には、操作した時点のアカウントのメールアドレス（ログイン ID）が記録されています。閲覧者への応答では、次のように変換して返します。管理者と CLI への応答は記録された値のままです。

- 既存のアカウントのメールアドレスは、そのアカウントの現在の表示名にします。
- どのアカウントのものでもないメールアドレス（アドレスを変えた、または削除したアカウントのもの）は空の文字列にします。
- `@` を含まない値（`system`、`cli`、`local:<OS のユーザー名>`）はそのまま返します。

## 2. 認証

この章のうち `GET /auth/me` 以外は、ブラウザ用（`/api/v1`）だけにあります（`GET /ui/modules` を含む）。

| メソッド | パス | 権限 | 要求 | 応答 |
| --- | --- | --- | --- | --- |
| GET | `/health` | なし | - | `{"status":"ok","version":"0.1.0","remember_days":30,"trust_days":30}`（`remember_days` は `session_remember_days`、`trust_days` は `two_step_trust_days` の値） |
| POST | `/auth/login` | なし | `{"email","password","remember"?}` | 下の「ログイン」。`email` はメールアドレス（大文字と小文字を区別しない）。`remember` が true なら有効期限付きの Cookie（ログインを保持） |
| POST | `/auth/login/verify` | なし | `{"challenge","code","trust"?}` | 下の「ログイン」。`trust` は「N 日間このブラウザを信頼する」（N は `trust_days`） |
| POST | `/auth/logout` | V | - | `{}` |
| GET | `/auth/me` | V | - | `Me`。`/admin/v1` では `{"id":0,"email":"","display_name":"cli","role":"admin","language":""}` |
| POST | `/auth/password` | V | `{"current_password","new_password"}` | `{}`（自分のパスワードの変更。他のセッション、確認済みのブラウザ、送信済みの確認コードは無効になる） |
| PUT | `/auth/language` | V | `{"language":"auto"\|"en"\|"ja"}` | `Me`（自分の表示言語の設定の変更） |
| PUT | `/auth/display-name` | V | `{"display_name"}` | `Me`（自分の表示名の変更。何も無効にならない） |
| POST | `/auth/email` | V | `{"email"}` | 下の「自分のメールアドレスの変更」 |
| POST | `/auth/email/verify` | V | `{"code"}` | 下の「自分のメールアドレスの変更」 |
| POST | `/auth/step-up` | A | - | 下の「再認証」 |
| POST | `/auth/step-up/verify` | A | `{"code"}` | 下の「再認証」 |
| GET | `/ui/modules` | V | - | `string[]`。画面が読み込む追加のモジュールの URL（同じオリジンの絶対パス）。閲覧者には `[]`、管理者には管理者用のエントリのファイル（例: `["/assets/CxJdEnzI.js"]`）。ビルドのマニフェストが無い、または管理者用のエントリが無い場合は全員に `[]`（[システム仕様.md](システム仕様.md) の 13.1.1） |

```ts
// language: 'auto' はブラウザの言語から自動判別（日本語なら ja、それ以外は en）
interface Me { id: number; email: string; display_name: string; role: 'admin' | 'viewer'; language: 'auto' | 'en' | 'ja' }
```

ログイン（`POST /auth/login` と `POST /auth/login/verify`）:

| 要求 | 状態 | 応答 |
| --- | --- | --- |
| `/auth/login` | 200 | `Me`。ログインした（開発モード、または確認済みのブラウザ）。セッションの Cookie を設定 |
| `/auth/login` | 202 | `{"two_step":true,"challenge":"<値>"}`。確認コードをアカウントのメールアドレスへ送った。challenge は `/auth/login/verify` に渡す値で、中身に意味は無い |
| `/auth/login` | 401 | `{"error":"invalid e-mail address or password"}`（存在しないアドレス、パスワードの誤り、無効なアカウント） |
| `/auth/login` | 423 | ロック中（`{"error":"login is temporarily locked after repeated failures"}`） |
| `/auth/login` | 503 | `{"error":"the verification code could not be sent; contact an administrator"}`。ログインは失敗し、セッションもコードも作らない |
| `/auth/login/verify` | 200 | `Me`。ログインした。セッションの Cookie と `mimamori_verify` の Cookie を設定 |
| `/auth/login/verify` | 401 | `{"error":"invalid code"}`。誤りはパスワードの誤りと同じ失敗の回数に数える |
| `/auth/login/verify` | 423 | ロック中、またはこの誤りでロックした |
| `/auth/login/verify` | 410 | `{"error":"the verification code has expired; log in again"}`。challenge が不明、期限切れ、使用済み、誤りの上限に達した、またはパスワードの入力からやり直して置き換わった。パスワードの入力からやり直す |

- コードは 6 桁の数字で、有効期限は 10 分、1 つのコードで誤りは 5 回までです。再送の要求はありません（パスワードの入力からやり直す）。
- Cookie `mimamori_verify`（HttpOnly、SameSite=Strict、Path `/api/v1/auth/login`）は、`trust` の値に関係なく `Max-Age` が `trust_days` 日です。この Cookie があるブラウザで次にログインするときにコードを省くかどうかは、セントラルが判定します（[システム仕様.md](システム仕様.md) の 12.4）。

自分のメールアドレスの変更（`POST /auth/email` と `POST /auth/email/verify`）:

| 要求 | 状態 | 応答 |
| --- | --- | --- |
| `/auth/email` | 200 | `{"applied":true}`。開発モードのため、すぐに変更した |
| `/auth/email` | 202 | `{"applied":false}`。確認コードを新しいアドレスへ送った |
| `/auth/email` | 400 | アドレスの誤り、または今のアドレスと同じ |
| `/auth/email` | 409 | ほかのアカウントのアドレス（大文字と小文字を区別しない） |
| `/auth/email` | 503 | `{"error":"the verification code could not be sent; contact an administrator"}` |
| `/auth/email/verify` | 200 | `Me`（変更後）。同じアカウントの他のセッション、確認済みのブラウザ、送信済みの確認コードは無効になる |
| `/auth/email/verify` | 400 | `{"error":"invalid code"}`（401 は使わない。ログインの失敗の回数にも数えない） |
| `/auth/email/verify` | 409 | 確認の間にほかのアカウントが同じアドレスを使った |
| `/auth/email/verify` | 410 | 依頼が無い、期限切れ、または誤りが 5 回になって依頼を破棄した |

- 現在のパスワードは求めません。確定できるのは、依頼したのと同じセッションだけです。新しい依頼は、確定していない古い依頼を置き換えます。

再認証（`POST /auth/step-up` と `POST /auth/step-up/verify`）:

コマンドセットの作成、変更、削除、インポート（4.1 章の「A（再認証）」）は、ブラウザからは、そのセッションで再認証のコードを入力した後の一定の時間だけ受け付けます（[システム仕様.md](システム仕様.md) の 12.5.10）。それ以外のときは、その操作の要求に次の応答を返し、何も変更しません。画面はコードを求めてから同じ要求をやり直します。

```json
{"error":"re-authentication required: request a verification code by mail and enter it to change command sets","step_up_required":true}
```

（状態は `428`。ログインのときに入力したコードは数えない。`/admin/v1` と開発モードでは返さない）

| 要求 | 状態 | 応答 |
| --- | --- | --- |
| `/auth/step-up` | 200 | `{"required":false}`。開発モードのため、再認証は要らない（何も送らない） |
| `/auth/step-up` | 202 | `{"required":true}`。確認コードをアカウントのメールアドレスへ送った。このセッションの前のコードは置き換わる |
| `/auth/step-up` | 503 | `{"error":"the verification code could not be sent; contact an administrator"}`。コードは作らない（履歴に `step_up_code_failed`） |
| `/auth/step-up/verify` | 200 | `{"required":true}`。再認証した（開発モードでは `{"required":false}` で何もしない） |
| `/auth/step-up/verify` | 400 | `{"error":"invalid code"}`（401 は使わない。ログインの失敗の回数にも数えない） |
| `/auth/step-up/verify` | 410 | `{"error":"the verification code has expired or was entered wrongly too often; request a new code"}`。コードが無い（このセッションで求めていない、使用済み）、期限切れ、または誤りが 5 回になって破棄した |

- コードの条件（6 桁、有効期限 10 分、誤りは 5 回まで）はログインと同じです。入力できるのはコードを求めたセッションだけで、1 つのコードで成功するのは 1 回だけです。
- 管理者のルートです（閲覧者には存在しない）。本文は無視します。

## 3. 概要

| GET | `/overview` | V | - | `Overview` |
| --- | --- | --- | --- | --- |

```ts
interface Overview {
  version: string;
  central_hostname: string;
  builtin_agent: { running: boolean; message: string };   // 停止中の理由を message に入れる
  agents: number; unacked_alerts: number;
}
```

## 4. エージェント

| メソッド | パス | 権限 | 応答 |
| --- | --- | --- | --- |
| GET | `/agents` | V | `AgentSummary[]` |
| GET | `/agents/{id}` | V | `AgentDetail` |

```ts
type ApplyStatus = 'none' | 'pending' | 'ok' | 'warning' | 'error';
interface AgentSummary {
  id: number;
  kind: 'builtin' | 'token';
  display_name: string;              // トークン名、または内蔵エージェントの固定の識別名 "(central)"
  hostname: string;
  token: TokenRef | null;            // 内蔵エージェントは null
  last_report_at: string | null;
  seconds_since_report: number | null;
  rule: {                            // 適用（割り当て）中の監視ルール。無ければ null
    id: number; name: string;
    assigned_version: number;        // 管理者が適用した版
    reported_version: number | null; // エージェントが適用を報告した版
    latest_version: number;          // 保存済みの最新の版
    outdated: boolean;               // assigned_version != latest_version
  } | null;
  apply: { status: ApplyStatus; message: string; version: number | null; at: string | null; warnings: string[] };
  unacked_alerts: number;
  auditd: AuditdStatus | null;
  ca_up_to_date: boolean;            // 最新の独自CAを受け取っているか（一度も同期していないエージェントは true）
  builtin_running: boolean | null;   // 内蔵エージェントのみ
}
interface TokenRef { id: number; name: string; status: TokenStatus; expires_at: string }
interface AuditdStatus {
  running: boolean; enabled: number; pid: number; lost: number; backlog: number;
  backlog_limit: number; backlog_wait_time: number; failure: number;
  never_rule: boolean;                                  // 監査を抑制するルールの有無
  halt_actions: string[];                               // ホストを停止させる設定（警告用）
}
interface AgentDetail extends AgentSummary {
  host_info: HostInfo | null;
  token_history: HistoryItem[];
  dropped_total: number;             // 保存量の上限で捨てた検知の累計
}
interface HostInfo {
  collected_at: string; hostname: string; os_name: string; os_version: string; kernel: string;
  agent_version: string;
  auditd: AuditdStatus;
  scan_base: string;
  directories: { path: string; owner: string; group: string; mode: string; depth: number }[];
  directories_truncated: boolean;
  rule: { id: number; version: number } | null;
  last_apply: { status: ApplyStatus; message: string; version: number | null; at: string | null; warnings: string[] };
  trusted_cas: { fingerprint: string; not_after: string }[];
}
```

## 4.1 コマンドセット

監視対象のホストでエージェントが実行するコマンドの組（コマンドセット）の定義、手動の実行指示、結果です。定義は監視対象ごとに持ちます（監視対象をまたいだ定義の一覧はありません）。動作は [システム仕様.md](システム仕様.md) の 12.5 を参照してください。

| メソッド | パス | 権限 | 要求 | 応答 |
| --- | --- | --- | --- | --- |
| GET | `/agents/{id}/command-sets` | A | - | `CommandSetList` |
| POST | `/agents/{id}/command-sets` | A（再認証） | `CommandSetSpec` | `CommandSet`（作成。`os_user` を省略すると、エージェントが報告した実行ユーザー） |
| POST | `/agents/{id}/command-sets/validate?set_id=` | A | `CommandSetSpec` | `{"errors":string[],"definition":CommandSetSpec \| null}`（保存しないで検証する） |
| POST | `/agents/{id}/command-sets/import` | A（再認証） | `{"source_agent_id","items":[{"set_id","name"?}]}` | `{"sets":CommandSet[]}` |
| GET | `/agents/{id}/command-sets/{set}` | A | - | `CommandSet` |
| PUT | `/agents/{id}/command-sets/{set}` | A（再認証） | `CommandSetSpec` | `CommandSet`（変更。`os_user` を省略すると、そのセットの現在の値） |
| DELETE | `/agents/{id}/command-sets/{set}` | A（再認証） | - | `{}`（論理削除） |
| POST | `/agents/{id}/command-sets/{set}/run` | A | - | `CommandRequest`（手動の実行指示） |
| DELETE | `/agents/{id}/command-requests/{req}` | A | - | `CommandRequest`（まだ配信していない指示の破棄） |
| GET | `/agents/{id}/command-results` | V | - | `{"agent_id","agent_name","results":CommandLatest[],"sets":CommandLatest[]}` |
| GET | `/command-results?name=` | V | - | `{"names":string[],"name":string,"results":CommandLatest[]}` |
| GET | `/agents/{id}/command-sets/{set}/runs?limit=&before=` | V | - | `CommandRunList` |
| GET | `/agents/{id}/command-runs/{run}` | V | - | `CommandRunDetail` |

- 「A（再認証）」の 4 つの操作（作成、変更、削除、インポート）は、ブラウザからは再認証の対象で、再認証が必要な場合は、要求の内容を確かめる前に 428 を返します（応答は 2 章の「再認証」、動作は [システム仕様.md](システム仕様.md) の 12.5.10）。`/admin/v1`（CLI）、開発モード、検証、実行指示と破棄は対象外です。
- 定義の要求（`CommandSetSpec`）は JSON のオブジェクト 1 つです。未知のフィールド、型の誤り、不正な値、上限を超える入力は 400 で、本文は `{"error":"invalid command set: <問題>; <問題>","errors":["<問題>", ...]}` です（各問題は `name: ...`、`command 2: timeout_sec: ...` のように項目と、コマンドなら位置（1 から）を示します）。日時を指定した 1 回のスケジュールで、過去の日時は 400 です（変更で、保存済みと同じ日時のままなら可）。
- 名前が、その監視対象の削除されていない別のセットと同じ場合は 409 です。
- `validate` は保存しないで検証し、問題を `errors` に返します（名前の重複を含む。変更の画面では `set_id` に対象のセットの ID を指定すると、そのセット自身との重複を問題にしません）。JSON として解釈できない場合も 200 で、`errors` に 1 件、`definition` は `null` です。`definition` は既定値を補った定義です。
- インポートは、`source_agent_id` の監視対象（`{id}` と同じでもよい）の削除されていないセットを、OS のユーザーと作業ディレクトリを含めてすべて写します。`name` を省略するか空にすると元の名前です。写し先の削除されていないセットの名前、またはインポートの中で名前が重なると 409 で、本文は `{"error":"...","conflicts":["<名前>", ...]}`、何も作りません。存在しない、または削除されたセットと、不正な新しい名前は 400 です。作ったセットの `imported_from` は「<元の監視対象名> / <元のセット名>」です。
- 実行指示は、そのセットにまだ応答の無い指示（`pending` または `delivered`）があれば 409 です。削除されたセットは 404 です。セットを削除すると、まだ配信していない指示は破棄します。
- 破棄は `pending`（まだエージェントに渡していない）の指示だけで、それ以外は 409 です。
- `/agents/{id}/command-results` の `results` は出力収集のセット、`sets` はすべてのセットの、最後に終わった実行です。`/command-results` の `names` は、すべての監視対象の削除されていない出力収集のセットの名前（重複なし、順に並べたもの）、`results` は `name` と同じ名前の出力収集のセットを持つ監視対象ごとの最後に終わった実行です（`name` が空なら空の配列）。
- 実行の一覧は新しい順で、`limit` は既定 50、最大 500、`before` はその ID より前の実行だけを返します（次のページは `next_before` を `before` に指定。最後のページでは `null`）。削除したセットの実行も、保存期間の間は見られます。
- 閲覧者への応答には、定義の項目（コマンドライン、備考、OS のユーザー、作業ディレクトリ、スケジュール、用途、説明、正規表現、戻り値の指定）と、コマンドごとに取り出した値（`values`）を含めません。下の型で「A のみ」と書いた項目は、閲覧者への応答ではキーごと省きます。管理者と CLI には全部を返します。
- コマンドセットの作成、変更、削除、インポート、実行指示、破棄、指示の期限切れは、履歴の分類 `command` に記録します。

```ts
type CodeSpec = { kind: 'single' | 'range' | 'min' | 'max'; a: number; b?: number };   // 0〜255。b は range だけ（a < b）
type CommandSchedule =
  | { kind: 'once'; at: string }                         // RFC3339（オフセット付き）。UTC に正規化して返す
  | { kind: 'weekly'; weekdays: ('sun'|'mon'|'tue'|'wed'|'thu'|'fri'|'sat')[]; time: string }   // time は "HH:MM"
  | { kind: 'daily'; time: string }
  | { kind: 'every_hours'; every: 1 | 2 | 3 | 4 | 6 | 8 | 12 }
  | { kind: 'every_minutes'; every: 1 | 2 | 3 | 4 | 5 | 6 | 10 | 12 | 15 | 20 | 30 }
  | { kind: 'hourly_at'; minute: number };               // 0〜59
// once 以外は監視対象のホストのローカル時刻で解釈する
interface CommandSpec {
  command_line: string;               // bash -c にそのまま渡す。空、NUL を含む、102400 バイト超は 400
  note: string;                       // 1000 文字まで
  success: CodeSpec;                  // 省略時 {kind:'single',a:0}
  failure: CodeSpec;                  // 省略時 {kind:'min',a:1}。success と重なると 400
  on_problems: 'stop' | 'continue_on_failure' | 'continue_always';   // 省略時 'stop'
  timeout_sec: number;                // 1〜86400、省略時 300
  line_limit: number;                 // 1〜10000、省略時 20（出力収集だけで使う）
  patterns: string[];                 // 正規表現（名前付きグループが必須）。出力収集だけ（実行のみで指定すると 400）
}
interface CommandSetSpec {
  name: string;                       // 1〜100 文字、制御文字なし。監視対象の中で一意
  description: string;                // 1000 文字まで
  purpose: 'collect_output' | 'run_only';
  os_user: string;                    // 1〜256 文字、空白と制御文字なし、先頭が '-' でない
  work_dir: string;                   // 空はそのユーザーのホーム。それ以外は絶対パス
  run_mode: 'manual' | 'schedule';    // 省略時は schedule の有無から決める
  schedule: CommandSchedule | null;   // manual では null
  keep_runs: number;                  // 1〜10000、省略時 100
  joiner: 'comma' | 'newline';        // 省略時 'comma'
  commands: CommandSpec[];            // 1 つ以上
}
interface CommandSet extends CommandSetSpec {
  id: number;
  revision: number;                   // 保存のたびに増える
  schedule_summary: string;           // スケジュールの英語の 1 行の要約（"manual"、"daily at 03:00 (host time JST, UTC+09:00)" など）
  imported_from: string;              // インポートしたセットの元。それ以外は ""
  created_at: string; created_by: string; updated_at: string; updated_by: string;
  request: CommandRequest | null;     // まだ応答の無い実行指示（pending / delivered）
  last_run: CommandRun | null;        // 最後に終わった実行
  running: boolean;                   // 実行中の実行がある
  agent_problems: string[];           // エージェントがこのセットを受け取らなかった理由（現在の定義について報告されたもの）
}
interface CommandSetList {
  agent_id: number; agent_name: string;
  default_os_user: string;            // エージェントが報告した実行ユーザー（新しいセットの OS のユーザーの既定値）。一度も報告が無ければ ""
  time_zone: string;                  // ホストのタイムゾーンの略称（"JST" など）。報告が無ければ ""
  utc_offset_sec: number;             // ホストの UTC からのずれ（秒）
  definitions_delivered: boolean;     // エージェントが現在の定義を受け取ったと報告している
  sets: CommandSet[];                 // 削除されていないセット（名前の順）
}
interface CommandRequest {
  id: number; set_id: number;
  state: 'pending' | 'delivered' | 'done' | 'expired' | 'discarded';
  requested_at: string; requested_by: string;
  expires_at: string;                 // requested_at の 24 時間後。それまでに配信しなければ expired
  delivered_at: string | null; done_at: string | null;
  run_uid: string;                    // done のとき、エージェントが始めた実行の一意な ID
  discarded_at: string | null; discarded_by: string;
}
interface DisplayValue {
  rows: { name: string; values: string[]; value: string }[];   // グループ名ごとに 1 行（最初に出た順）。name が "" の行は、正規表現の無いコマンドの出力。value は values をセットのつなぎ方で連結したもの
  truncated: boolean;                 // 行数の上限で切り詰めた
}
interface CommandRun {
  id: number; set_id: number;
  set_name: string;                   // 実行したときの名前
  trigger: 'manual' | 'schedule';
  scheduled_for: string | null;       // 定期の予定時刻
  request_id: number;                 // 手動の実行指示の ID（定期は 0）
  commands_total: number;
  started_at: string; finished_at: string | null;   // 実行中は null
  state: 'running' | 'success' | 'failure' | 'error';   // 終わった実行は、実行したコマンドの最も悪い区分
  reason: string;                     // 実行全体の理由（"interrupted" など。通常は ""）
  display: DisplayValue | null;       // 出力収集の終わった実行だけ
  received_at: string;                // 最初の報告を受け取った日時（保存期間の基準）
  set_revision?: number; purpose?: string; os_user?: string; work_dir?: string;   // A のみ。実行したときの値
}
interface CommandLatest {
  agent_id: number; agent_name: string; set_id: number; set_name: string;
  running: boolean;
  last_run: CommandRun | null;        // 最後に終わった実行（区分を問わない）
  purpose?: string;                   // A のみ
}
interface CommandRunList {
  agent_id: number; agent_name: string; set_id: number; set_name: string;
  deleted: boolean;                   // セットは削除済み
  runs: CommandRun[];
  next_before: number | null;
  purpose?: string;                   // A のみ
}
interface CommandRunDetail extends CommandRun {
  agent_id: number; agent_name: string;
  not_executed: number;               // セットが止まったため実行しなかったコマンドの数（終わった実行だけ）
  commands: {
    position: number;                 // セットの中の位置（0 から）
    kind: 'success' | 'failure' | 'error';
    reason: string;                   // error の理由: unexpected_code、start_failed、workdir_failed、timeout、signaled、interrupted
    exit_code: number | null;         // 正常に終了しなかった場合は null
    started_at: string; finished_at: string;
    output: string;                   // コンソール出力（1 MiB まで。不正な UTF-8 は置換文字にする）
    output_bytes: number;             // コマンドが出力した全体の大きさ
    output_truncated: boolean;
    command_line?: string; note?: string;   // A のみ。実行したときの値
    values?: { values: { name: string; value: string }[]; truncated: boolean; pattern_problems?: { pattern: number; problem: 'timeout' | 'invalid' }[] };   // A のみ。出力収集のとき
  }[];
}
```

## 5. トークン

| メソッド | パス | 権限 | 要求 | 応答 |
| --- | --- | --- | --- | --- |
| GET | `/tokens` | V | - | `Token[]` |
| GET | `/tokens/{id}` | V | - | `Token` |
| POST | `/tokens` | A | `{"name","valid_days"?,"rule_id"?,"rule_version"?}` | `{"token":Token,"value":"mmr_..."}` |
| POST | `/tokens/{id}/reissue` | A | `{"valid_days"?}` | `{"token":Token,"value":"mmr_..."}` |
| POST | `/tokens/{id}/unbind` | A | - | `Token` |
| POST | `/tokens/{id}/revoke` | A | - | `Token` |

```ts
type TokenStatus = 'unused' | 'active' | 'expired' | 'revoked';
interface Token {
  id: number; name: string;
  value: string;                     // /admin/v1（CLI）は値の全体、/api/v1（ブラウザ）は略した形 "mmr_AbCd...WxYz"（発行と再発行の応答の外側の value は除く）
  status: TokenStatus;
  expires_at: string; created_at: string; created_by: string;
  last_seen_at: string | null; instance_id: string; bound_at: string | null;
  reissued_at: string | null; renewed_at: string | null;
  pending_renewal: boolean;          // 自動更新の新しい値を渡し済みで、まだ使われていない
  expiring_unconnected: boolean;     // 期限が近いのにエージェントが接続していない
  agent_id: number;
}
```

- 発行と再発行の応答の `value` は、新しい値の全体です（ブラウザにも返す。管理者が画面を閉じるまで表示してコピーするため）。それ以外の応答では、ブラウザには略した形だけを返します。
- `valid_days` を省略または 0 にすると設定の既定値を使います。指定する場合は 1〜3650 で、範囲外は 400 です。トークン名は `[A-Za-z0-9._-]{1,64}` で一意です。

## 6. 監視ルール

| メソッド | パス | 権限 | 要求 | 応答 |
| --- | --- | --- | --- | --- |
| GET | `/rules` | V | - | `RuleSummary[]` |
| GET | `/rules/{id}` | V | - | `RuleDetail`（最新の版の内容） |
| GET | `/rules/{id}/versions/{version}` | V | - | `RuleVersion` |
| GET | `/rules/{id}/diff?from=N&to=M` | V | - | `{"from":N,"to":M,"lines":DiffLine[]}` |
| POST | `/rules/merge-preview` | A | `{"templates":["base","nginx"],"choices"?:{field:template}}` | `MergePreview`（`choices` で解決した衝突は `conflicts` に含めない） |
| POST | `/rules` | A | `RuleCreate` | `RuleDetail`（衝突のあるテンプレートで `choices` が足りないと 400） |
| PUT | `/rules/{id}` | A | `{"description"?,"content","comment"?}` | `RuleDetail`（版が 1 上がる。`description` を省略すると現在の説明を保つ） |
| POST | `/rules/{id}/apply` | A | `{"version"?,"agent_ids":[1,2]}` | `{"applied":2}`（`version` を省略または 0 にすると最新の版。重複した ID は 1 件として扱う。`agent_ids` が空、または存在しない ID や版があると 400） |
| POST | `/rules/{id}/to-template` | A | `{"version"?,"name","description"?}` | `TemplateDetail`（`version` を省略または 0 にすると最新の版。名前の形式の誤りは 400、組み込みと同名または既存と重複は 409） |
| DELETE | `/rules/{id}` | A | - | `{}`（適用中なら 409） |
| POST | `/rules/validate` | A | `{"content":RuleContent}` | `{"errors":string[]}` |

```ts
type Category = 'alert' | 'normal' | '';          // '' は「指定なし」
interface Element { value: string; if_missing: 'ignore' | 'warn' }
interface LegitLocation { path: string; extensions: string[]; category: 'alert' | 'normal' }   // category は必須（空は 400）
interface RuleContent {
  users: Element[];                                // OSのユーザー（実行、拒否された操作、読み込み、書き込み可能な状態で使う）
  exclude_interactive: boolean | null;             // true: ログインした利用者の操作と cron を対象外にする
  check_interval_sec: number;                      // 0 は指定なし（既定 60）
  scan_interval_sec: number;                       // 書き込み可能な状態の走査の間隔。0 は指定なし（既定 3600）
  exec: { category: Category };
  denied: { category: Category };
  read: { dirs: Element[]; category: Category };
  network: { executables: Element[]; category: Category };
  writable: { base: string; allowed_paths: string[]; category: Category };   // base が走査の起点
  exec_files: { extensions: string[]; legit: LegitLocation[]; other_category: Category };
}
interface RuleSummary {
  id: number; name: string; description: string; latest_version: number;
  created_at: string; updated_at: string;
  applied_agents: { agent_id: number; display_name: string; version: number }[];
}
interface RuleDetail extends RuleSummary {
  content: RuleContent;
  versions: { version: number; created_at: string; created_by: string; comment: string }[];
}
interface RuleVersion { rule_id: number; version: number; content: RuleContent; created_at: string; created_by: string; comment: string }
interface DiffLine { op: ' ' | '+' | '-'; text: string }
interface MergePreview {
  content: RuleContent;                            // 衝突の無い項目をまとめた内容（衝突項目は最初のテンプレートの値）
  conflicts: { field: string; options: { template: string; value: unknown }[] }[];
}
interface RuleCreate {
  name: string; description: string;
  templates?: string[];                            // 指定時はテンプレートを展開する
  choices?: Record<string, string>;               // 衝突した field ごとに採用するテンプレート名
  content?: RuleContent;                           // templates を指定しない場合の内容（省略で空）
}
```

- `content`（監視ルールの作成と保存、テンプレートの作成と変更、`/rules/validate`）は JSON のオブジェクトである必要があります。`null`、配列、文字列などは 400 です。

- 種類ごとの有効条件（項目を指定しなかった種類は行わない）:
  - 実行: `users` と `exec.category`
  - 拒否された操作: `users` と `denied.category`
  - 読み込み: `users` と `read.dirs` と `read.category`
  - 通信: `network.executables` と `network.category`
  - 書き込み可能な状態: `writable.base` と `writable.category`
  - 実行されるファイル: `writable.base` と `writable.allowed_paths` と `exec_files.extensions` と `exec_files.other_category`
- 入力の検証（違反は 400）:
  - 監視ルール名は制御文字を除く 1〜100 文字です。
  - `check_interval_sec` は 0 または 10〜3600、`scan_interval_sec` は 0 または 60〜604800 です。
  - `users`、`read.dirs`、`network.executables`、`writable.allowed_paths`、`exec_files.legit` は各 200 件まで、`exec_files.extensions` は 50 件までです。
  - `writable.base` は絶対パスで、`/` は使えません。`read.dirs` と `network.executables` も絶対パスです。
  - `exec_files.legit` の各項目は、`extensions` を 1 つ以上と、`category`（`alert` / `normal`）が必要です。
  - 全体の検証の内容は [システム仕様.md](システム仕様.md) の 7 章を参照してください。
- 衝突の field の値: `exclude_interactive`、`check_interval_sec`、`scan_interval_sec`、`exec.category`、`denied.category`、`read.category`、`network.category`、`writable.base`、`writable.category`、`exec_files.other_category`、`exec_files.legit:<path>|<ext,ext>`。

## 7. テンプレート

| メソッド | パス | 権限 | 要求 | 応答 |
| --- | --- | --- | --- | --- |
| GET | `/templates` | V | - | `TemplateSummary[]` |
| GET | `/templates/{name}` | V | - | `TemplateDetail` |
| POST | `/templates` | A | `{"name","description","content"?,"from_builtin"?,"from_rule"?:{"rule_id","version"?}}` | `TemplateDetail`（`from_rule.version` が 0 または省略なら最新の版。名前は `[A-Za-z0-9._-]{1,64}`、組み込みと同名または既存と重複は 409） |
| PUT | `/templates/{name}` | A | `{"description"?,"content"}` | `TemplateDetail`（`description` を省略すると現在の説明を保つ。組み込みは 403） |
| DELETE | `/templates/{name}` | A | - | `{}`（組み込みは 403） |

```ts
interface TemplateSummary { name: string; builtin: boolean; description: string; updated_at: string | null }
interface TemplateDetail extends TemplateSummary { content: RuleContent }
```

## 8. 検知

| メソッド | パス | 権限 | 要求 | 応答 |
| --- | --- | --- | --- | --- |
| GET | `/detections` | V | クエリ（下記） | `{"items":Detection[],"total":N}` |
| GET | `/detections/{agent_id}/{id}` | V | - | `Detection` |
| POST | `/detections/ack` | A | `{"items":[{"agent_id","id"}]}` | `{"acknowledged":N}` |

クエリ: `agent_id`、`category`（alert/normal）、`type`、`from`、`to`（最後の発生時刻で絞り込み）、`acknowledged`（true/false）、`suppressed`（true/false）、`limit`（既定 100、最大 500）、`offset`（0 以上、最大 1000000）。

`from` と `to` は RFC3339、または `YYYY-MM-DD`、`YYYY-MM-DDTHH:MM`、`YYYY-MM-DDTHH:MM:SS`（ローカル時間）です。`acknowledged` と `suppressed` は `true` / `false`（`1` / `0` も可）です。`agent_id`、`limit`、`offset` は整数で、`limit` と `offset` は範囲外なら範囲内に丸めます。`YYYY-MM-DDTHH:MM[:SS]` と `YYYY-MM-DD` のようにタイムゾーンを持たない値は、セントラルのローカル時間として解釈します。空の値は指定なしとして扱います。解釈できない値は、その項目名を示すメッセージとともに 400 を返します。

```ts
type DetectionType = 'exec' | 'denied' | 'read' | 'network' | 'writable' | 'exec_file';
interface Detection {
  id: number; agent_id: number; agent_name: string; hostname: string;
  type: DetectionType; category: 'alert' | 'normal';
  os_user: string; exe: string; target: string;
  count: number; first_at: string; last_at: string;
  file: { ctime: string | null; owner: string; mode: string } | null;   // writable と exec_file だけ。ctime は不明なら null
  suppressed: boolean; suppression_id: number | null;
  acknowledged_at: string | null; acknowledged_by: string;
  notify_state: 'pending' | 'sent' | 'skipped';
}
```

検知の id は監視対象ごとの DB 内で採番されるため、`agent_id` と組で特定します。

## 9. 抑止条件

| メソッド | パス | 権限 | 要求 | 応答 |
| --- | --- | --- | --- | --- |
| GET | `/suppressions` | V | - | `Suppression[]` |
| POST | `/suppressions` | A | `{"agent_id"?,"type"?,"os_user"?,"exe"?,"target"?,"comment"?}` | `Suppression`（作成時に、条件に一致する未確認の検知へ抑止を即時に反映する） |
| DELETE | `/suppressions/{id}` | A | - | `{}` |

```ts
interface Suppression {
  id: number; agent_id: number | null; agent_name: string;   // null は全エージェント
  type: DetectionType | ''; os_user: string; exe: string; target: string;   // '' は任意。exe と target は * を使える
  comment: string; created_at: string; created_by: string;
}
```

作成時の検証: 少なくとも 1 つの条件（`type`、`os_user`、`exe`、`target`）が必要で、すべて空なら 400 です。未知の `type` と存在しない `agent_id` も 400 です。`os_user`、`exe`、`target`、`comment` は前後の空白を除いた値で、制御文字（NUL、CR、LF、タブなど）を含む場合、または上限（`exe` と `target` は 4096 バイト、`os_user` は 256 バイト、`comment` は 1000 文字）を超える場合は 400 を返します。

## 10. 証明書

| GET | `/certificates` | V | - | `CertStatus` |
| --- | --- | --- | --- | --- |

```ts
interface CertInfo { fingerprint: string; subject: string; not_before: string; not_after: string; status: 'active' | 'next' | 'retired' }
interface CertStatus {
  cas: CertInfo[];
  server?: CertInfo & { names: string[]; issuer_fingerprint: string };   // 有効なサーバー証明書が無いと省略
  rotation: { state: 'none' | 'distributing' | 'switched'; next_ca_created_at: string | null; switch_after: string | null };
  agents_missing_new_ca: { agent_id: number; display_name: string }[];   // 一度も同期していないエージェントは含めない
}
```

## 11. 履歴

| GET | `/history?category=&limit=&offset=` | V | - | `{"items":HistoryItem[],"total":N}` |
| --- | --- | --- | --- | --- |

`limit`（既定 100、最大 500）と `offset`（既定 0、最大 10000000）は整数で、範囲外は範囲内に丸めます。整数として解釈できない値は 400 を返します。

閲覧者には、分類が `account`（アカウント）、`config`（システム設定）、`command`（コマンドセットの変更と実行指示）の行を返しません（`total` にも数えません）。`category` にこれらを指定した場合は、空の一覧を返します。

```ts
type HistoryCategory = 'account' | 'rule' | 'token' | 'certificate' | 'rejected' | 'template' | 'suppression' | 'detection' | 'config' | 'command';
interface HistoryItem {
  id: number; at: string; account_name: string;   // 操作したアカウントのその時点のメールアドレス（閲覧者には 1.3 の変換をした値）。システムの処理は "system"、CLI（ctl）の操作は "cli"、セントラルのホスト上のコマンドは "local:<OS のユーザー名>"
  category: HistoryCategory; action: string; target: string; detail: string;
  remote_addr: string; count: number;            // 拒否した接続は同じ接続元、理由、トークン名（target）を 10 分以内ならまとめて数える
}
```

## 12. アカウント

| メソッド | パス | 権限 | 要求 | 応答 |
| --- | --- | --- | --- | --- |
| GET | `/accounts` | A | - | `Account[]` |
| POST | `/accounts` | A | `{"email","display_name","role","language"?,"password"}` | `Account`（`language` は `auto` / `en` / `ja`、省略時は `auto`。メールアドレスの重複は 409） |
| PUT | `/accounts/{id}/email` | A | `{"email"}` | `Account`（確認コードなしで即時に変更。対象のセッション、確認済みのブラウザ、送信済みの確認コードはすべて無効になる。重複は 409） |
| PUT | `/accounts/{id}/display-name` | A | `{"display_name"}` | `Account`（セッションはそのまま） |
| PUT | `/accounts/{id}/role` | A | `{"role"}` | `Account`（対象のセッションは無効になる） |
| PUT | `/accounts/{id}/notify` | A | `{"notify": boolean}` | `Account`（通知メールの宛先にするかどうか。セッションはそのまま。`notify` が無い、真偽値でない場合は 400） |
| PUT | `/accounts/{id}/password` | A | `{"password"}` | `Account`（対象のセッション、確認済みのブラウザ、送信済みの確認コードはすべて無効になる） |
| POST | `/accounts/{id}/disable` | A | - | `Account`（対象のセッション、確認済みのブラウザ、送信済みの確認コードはすべて無効になる） |
| POST | `/accounts/{id}/enable` | A | - | `Account` |
| DELETE | `/accounts/{id}` | A | - | `{}` |

```ts
interface Account {
  id: number; email: string; display_name: string; role: 'admin' | 'viewer'; language: 'auto' | 'en' | 'ja'; disabled: boolean;
  notify: boolean;                    // 通知メールを受け取る（作成時は false）。無効化の間は選択を保ったまま送らない
  locked_until: string | null; created_at: string;
}
```

- 有効な管理者が 1 人だけの場合、その管理者の削除、無効化、閲覧者への変更は 409 です（確認と変更は 1 つの書き込みのトランザクションで行うため、同時の要求でも有効な管理者が 0 人になることはありません）。
- メールアドレス（ログイン ID）は前後の空白を除いて 254 文字以下の、表示名や引用符の無い 1 つのアドレスです（空白、制御文字、`"` `<` `>` `,` は不可。ドメインのドットは不要で `user@localhost` も可）。大文字と小文字を区別せずに一意です。
- 表示名は前後の空白を除いて 1〜64 文字で、制御文字は使えません。重複してもかまいません。
- パスワードは 10 文字以上、256 バイトまでです。
- 通知メール（検知の通知）の宛先は、`notify` が `true` で無効化されていないアカウントのアドレスと、設定 `smtp.to` の追加のアドレスです（重複は除く。詳細は [システム仕様.md](システム仕様.md) の 12.2）。`notify` の変更は履歴（`account`、操作 `notify_changed`、詳細 `notification mails on` / `notification mails off`）に残します。値が変わらない要求は何も記録せずに `Account` を返します。

## 12.1 システム設定

| メソッド | パス | 権限 | 要求 | 応答 |
| --- | --- | --- | --- | --- |
| GET | `/config` | A | - | `CentralConfig` |
| PUT | `/config` | A | `{"set"?: {項目: 値}, "unset"?: [項目]}` | `CentralConfig`（変更後） |

```ts
interface CentralConfig { items: ConfigItem[] }
interface ConfigItem {
  name: string;                       // 例 "retention_days"、"smtp.host"
  kind: 'string' | 'int' | 'bool' | 'list' | 'secret';
  value: string;                      // 保存した値または既定値（一覧はカンマ区切り、真偽値は "true" / "false"。secret は常に ""）
  is_set: boolean;                    // secret に値があるか
  default: string;                    // 既定値（secret は ""）
  stored: boolean;                    // false は既定値で動作中
  restart: boolean;                   // true はセントラルの再起動後に反映
  pending: boolean;                   // 再起動後に反映する項目で、保存した値がまだ使われていない
}
```

- `set` と `unset`（既定値に戻す）はまとめて検証し、1 つのトランザクションで保存します。不正な値や組み合わせ、空に意味が無い項目の空の値は 400 で、何も保存しません（検証の内容は [システム仕様.md](システム仕様.md) の設定の表）。
- 即時反映の項目は保存と同時に反映します。変更した項目を、保存した値（一覧は正規化した値）で履歴（`config`）に残します（`smtp.password` は値を残さない）。

## 13. 導入の案内

| GET | `/guide` | V | - | `Guide` |
| --- | --- | --- | --- | --- |

```ts
interface Guide {
  version: string; central_hostname: string;
  central_hosts: string[];       // エージェントが接続に使えるセントラルのホスト名と IP アドレス（サーバー証明書の名前から、ループバックを除く）
  central_port: number;          // エージェントが接続するポート（動作中の agent_listen のポート）
  ca_fingerprint: string;        // 現在の独自CAの SHA-256 フィンガープリント
  default_valid_days: number;
}
```

## 14. エージェント用の API（`/agent/v1`）

| メソッド | パス | 認証 | 要求 | 応答 |
| --- | --- | --- | --- | --- |
| GET | `/agent/v1/ca` | なし | - | `{"ca_pems":["-----BEGIN CERTIFICATE-----..."]}` |
| POST | `/agent/v1/sync` | トークン | `SyncRequest` | `SyncResponse` |
| POST | `/agent/v1/report` | トークン | `ReportRequest` | `{}` |

- すべての認証付きの要求に `X-Mimamori-Instance-Id`（エージェントが初期化時に作ってローカル DB に保存するインスタンス ID）を付けます。トークンが無い、無効、期限切れ、失効、インスタンスIDの不一致、インスタンス ID が空または 128 文字を超える場合は、理由を返さずに 401 とし、履歴（`rejected`）に残します。
- 要求の本文が JSON として解釈できない場合は 400（`{"error":"invalid request"}`）、セントラルの内部エラーは 500（`{"error":"internal error"}`）です。未知のフィールドは無視します。
- この章の日時は、1 章の例外として Unix 秒（整数）です。`0` は未設定を表します。
- 通信の流れは [システム仕様.md](システム仕様.md) の 8 章を参照してください。

### 14.1 同期（`POST /agent/v1/sync`）

```ts
interface ApplyResult {
  status: 'none' | 'ok' | 'warning' | 'error';
  message: string;
  rule_id: number; version: number;   // 適用した監視ルールとその版
  at: number;                         // 適用した日時（Unix 秒）
  warnings: string[];
}
interface SyncRequest {
  agent_version: string;
  rule_id: number; rule_version: number;   // エージェントが適用中の監視ルールと版（無ければ 0）
  ca_fingerprints: string[];               // 信頼している独自 CA の SHA-256 フィンガープリント
  last_apply: ApplyResult | null;          // 最後の適用の結果
  command_sig: string;                     // 保持しているコマンドセットの定義の署名（無ければ ""）
}
interface RuleAssignment { id: number; name: string; version: number; content: RuleContent }   // RuleContent は 6 章
interface TokenRenewal { token: string; expires_at: number }
interface SyncResponse {
  agent_name: string;                      // トークン名。内蔵エージェントは "(central)"
  token_expires_at: number;                // 現在のトークンの有効期限。内蔵エージェントは 0
  rule: RuleAssignment | null;             // 割り当てた監視ルールと版。割り当てが無ければ null
  default_interval_sec: number;            // 監視ルールが無い場合の確認の間隔（秒）
  token_renewal?: TokenRenewal;            // 有効期限が近いときだけ。使われるまで同じ値を返す
  ca_pems?: string[];                      // 信頼していない独自 CA があるときだけ。信頼すべきすべての CA の証明書（PEM）
  command_sets?: {                         // command_sig がセントラルの署名と違うときだけ。その監視対象のすべての定義（削除されていないセット）
    signature: string;
    sets: (CommandSetSpec & { id: number; revision: number })[];   // CommandSetSpec は 4.1 章（schedule.at は RFC3339 の文字列）
  };
  command_requests?: { id: number; set_id: number; requested_at: number; expires_at: number }[];   // 応答を待っている手動の実行指示
}
```

- 定義の署名は、監視対象の DB を作ったときの乱数と、削除されていない各セットの ID と版から作ります（DB を作り直すと署名が変わり、定義を送り直します）。セントラルは、エージェントが送った `command_sig` を監視対象ごとに記録します（4.1 章の `definitions_delivered`）。
- 実行指示は、`pending` のうち期限（`expires_at`）前のものと、`delivered` のもの（エージェントが応答するまで毎回）を送ります。`pending` の指示は最初に送ったときに `delivered` にします。内蔵エージェントにも同じものを渡します。

### 14.2 報告（`POST /agent/v1/report`）

```ts
interface DetectionReport {
  type: DetectionType; category: 'alert' | 'normal';   // 未知の type の項目は捨てる。category が不正なら 'alert'
  os_user: string; exe: string; target: string;
  count: number;                       // 1 未満は 1
  first_at: number; last_at: number;   // Unix 秒。0 なら受信時刻（last_at は first_at）
  file_ctime?: number; file_owner?: string; file_mode?: string;   // writable と exec_file の検知
}
interface ReportRequest {
  created_at: number;                  // 報告を作った日時（Unix 秒）
  detections: DetectionReport[];
  host_info?: HostInfo;                // 4 章の HostInfo と同じ項目。日時（collected_at、trusted_cas[].not_after）は Unix 秒
  apply?: ApplyResult;                 // 最後の適用の結果（まだ適用していなければ省略）
  dropped: number;                     // 保存量の上限で捨て、まだ報告していない検知の件数
  command_runs?: CommandRunReport[];   // コマンドセットの実行（開始、コマンドごと、終了のたびに送る）
  command_request_acks?: { request_id: number; status: 'started' | 'expired' | 'unknown_set'; run_uid?: string; at: number }[];
  command_apply?: { signature: string; at: number; problems: { set_id: number; set_name: string; problems: string[] }[] };   // 配信された定義の受け取り結果
}
interface CommandRunReport {
  run_uid: string;                     // エージェントが作る実行の一意な ID（128 バイトまで）
  set_id: number; set_revision: number; set_name: string; purpose: string; joiner: string;
  os_user: string; work_dir: string;
  trigger: 'manual' | 'schedule'; request_id: number; scheduled_for: number;
  commands_total: number; started_at: number;
  finished: boolean;                   // 実行の最後の報告
  finished_at: number; state: 'success' | 'failure' | 'error'; reason: string;   // finished のとき
  display?: DisplayValue;              // 出力収集の finished のとき（4.1 章）
  results: {
    position: number; command_line: string; note: string;
    kind: 'success' | 'failure' | 'error'; reason: string; exit_code: number | null;
    started_at: number; finished_at: number;
    output: string;                    // コンソール出力のバイト列（base64）
    output_bytes: number; truncated: boolean;
    values?: { values: { name: string; value: string }[]; truncated: boolean; pattern_problems?: { pattern: number; problem: string }[] };
  }[];
}
```

- `ReportRequest` の `target` は 512 バイト、`exe` は 1024 バイトまでに切り詰めます。`apply.message` は 2000 バイトまでです。
- `host_info` の `os_user`（エージェントの実行ユーザー）、`time_zone`、`utc_offset_sec`（ホストのタイムゾーン）は監視対象ごとに記録し、新しいコマンドセットの OS のユーザーの既定値と、スケジュールの表示に使います。
- コマンドセットの実行は冪等に保存します。どの報告にも開始時の値があり、実行（`run_uid`）は最初に届いた報告から作ります。コマンドの結果は（実行、`position`）ごとに 1 回だけ保存し、実行は最初の `finished` の報告で 1 回だけ閉じます。`state` が 3 つの区分のどれでもなければ、保存済みのコマンドの区分の最も悪いもの（無ければ `error`）にします。監視対象の DB に無いセットの実行は捨てます。`request_id` のある実行を作ると、その指示を `done` にします。
- `command_request_acks` の `started` は指示を `done` に、`expired`（期限後に届いた）と `unknown_set`（そのセットを持っていない）は `expired` にします（後の 2 つは履歴に残す）。
- `command_apply` は監視対象ごとに記録し、署名が現在の定義と一致する間、受け取らなかったセットの理由を 4.1 章の `agent_problems` に示します。
- コマンドセットの結果について、メールで通知することはありません。
- 応答は `{}` です。
