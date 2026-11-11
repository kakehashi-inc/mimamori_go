# API 仕様 - mimamori

セントラルが提供する HTTP API の仕様です。ブラウザ画面（`frontend/`）、CLI、エージェントはすべて本書の API を使います。

## 1. 共通

### 1.1 待ち受けとパス

| 待ち受け | 通信 | パス | 利用者 | 認証 |
| --- | --- | --- | --- | --- |
| ブラウザ用（既定 `0.0.0.0:8080`） | HTTP | `/api/v1/*` | ブラウザ画面 | ログインセッション（Cookie `mimamori_session`） |
| ブラウザ用 | HTTP | `/*` | ブラウザ | なし（埋め込み SPA） |
| エージェントおよび管理用（既定 `0.0.0.0:8443`） | HTTPS（独自CA） | `/agent/v1/*` | エージェント | トークン（`Authorization: Bearer mmr_...`）とインスタンスID |
| エージェントおよび管理用 | HTTPS（独自CA） | `/admin/v1/*` | CLI | 接続元がループバックで、CLI 用の秘密（セントラルがデータディレクトリの `cli.secret` に書く値。`Authorization: Bearer <値>`） |

- `/api/v1/X` と `/admin/v1/X` は、認証の方法を除いて同じ操作です（以下ではパスを `/api/v1` を省略して記載します）。`/admin/v1` は CLI 用で、アカウントとは結び付かず最大の権限を持ちます。
- ループバック以外からの `/admin/v1/*` は 404 を返します。接続元の判定には TCP の接続元アドレスだけを使います。
- 1 接続で 1 リクエストを処理し、応答は `Connection: close` です。

### 1.2 形式

- 要求・応答は JSON（`Content-Type: application/json`）。日時は RFC3339（UTC）の文字列、未設定は `null`。
- エラー時は 4xx/5xx と `{"error": "<message>"}` を返します。
  - 400: 入力の誤り、401: 未認証、403: 権限なし、404: 存在しない、409: 状態の衝突（重複、適用中の監視ルールの削除など）、423: ログインの一時停止、503: 混雑。
- ブラウザ用の待ち受けで GET 以外を送る場合は、CSRF 対策としてヘッダー `X-Mimamori-Request: 1` を必須とします（無い場合は 403）。

### 1.3 権限

`A` は管理者のみ、`V` は管理者と閲覧者が使えます。権限の判定はすべてのリクエストでセントラルが行います。

## 2. 認証（ブラウザ用のみ）

| メソッド | パス | 権限 | 要求 | 応答 |
| --- | --- | --- | --- | --- |
| GET | `/health` | なし | - | `{"status":"ok","version":"0.1.0","remember_days":30}` |
| POST | `/auth/login` | なし | `{"name","password","remember"?}` | `Me`（Cookie を設定）。`remember` が true なら有効期限付きの Cookie（ログインを保持）。失敗は 401、一時停止中は 423 |
| POST | `/auth/logout` | V | - | `{}` |
| GET | `/auth/me` | V | - | `Me` |
| POST | `/auth/password` | V | `{"current_password","new_password"}` | `{}`（自分のパスワードの変更。他のセッションは無効になる） |
| PUT | `/auth/language` | V | `{"language":"auto"\|"en"\|"ja"}` | `Me`（自分の表示言語の設定の変更） |

```ts
// language: 'auto' はブラウザの言語から自動判別（日本語なら ja、それ以外は en）
interface Me { id: number; name: string; role: 'admin' | 'viewer'; language: 'auto' | 'en' | 'ja' }
```

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
  ca_up_to_date: boolean;            // 最新の独自CAを受け取っているか
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
  id: number; name: string; status: TokenStatus;
  expires_at: string; created_at: string; created_by: string;
  last_seen_at: string | null; instance_id: string; bound_at: string | null;
  reissued_at: string | null; renewed_at: string | null;
  pending_renewal: boolean;          // 自動更新の新しい値を渡し済みで、まだ使われていない
  expiring_unconnected: boolean;     // 期限が近いのにエージェントが接続していない
  agent_id: number;
}
```

- `value` は発行時と再発行時の応答にだけ含まれます。セントラルはハッシュ値だけを保存します。
- `valid_days` を省略すると設定の既定値を使います。トークン名は `[A-Za-z0-9._-]{1,64}` で一意です。

## 6. 監視ルール

| メソッド | パス | 権限 | 要求 | 応答 |
| --- | --- | --- | --- | --- |
| GET | `/rules` | V | - | `RuleSummary[]` |
| GET | `/rules/{id}` | V | - | `RuleDetail`（最新の版の内容） |
| GET | `/rules/{id}/versions/{version}` | V | - | `RuleVersion` |
| GET | `/rules/{id}/diff?from=N&to=M` | V | - | `{"from":N,"to":M,"lines":DiffLine[]}` |
| POST | `/rules/merge-preview` | A | `{"templates":["base","nginx"]}` | `MergePreview` |
| POST | `/rules` | A | `RuleCreate` | `RuleDetail` |
| PUT | `/rules/{id}` | A | `{"description","content","comment"}` | `RuleDetail`（版が 1 上がる） |
| POST | `/rules/{id}/apply` | A | `{"version","agent_ids":[1,2]}` | `{"applied":2}` |
| POST | `/rules/{id}/to-template` | A | `{"version","name","description"}` | `TemplateDetail` |
| DELETE | `/rules/{id}` | A | - | `{}`（適用中なら 409） |
| POST | `/rules/validate` | A | `{"content":RuleContent}` | `{"errors":string[]}` |

```ts
type Category = 'alert' | 'normal' | '';          // '' は「指定なし」
interface Element { value: string; if_missing: 'ignore' | 'warn' }
interface LegitLocation { path: string; extensions: string[]; category: Category }
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

- 種類ごとの有効条件（項目を指定しなかった種類は行わない）:
  - 実行: `users` と `exec.category`
  - 拒否された操作: `users` と `denied.category`
  - 読み込み: `users` と `read.dirs` と `read.category`
  - 通信: `network.executables` と `network.category`
  - 書き込み可能な状態: `writable.base` と `writable.category`
  - 実行されるファイル: `writable.base` と `writable.allowed_paths` と `exec_files.extensions` と `exec_files.other_category`
- 衝突の field の値: `exclude_interactive`、`check_interval_sec`、`scan_interval_sec`、`exec.category`、`denied.category`、`read.category`、`network.category`、`writable.base`、`writable.category`、`exec_files.other_category`、`exec_files.legit:<path>|<ext,ext>`。

## 7. テンプレート

| メソッド | パス | 権限 | 要求 | 応答 |
| --- | --- | --- | --- | --- |
| GET | `/templates` | V | - | `TemplateSummary[]` |
| GET | `/templates/{name}` | V | - | `TemplateDetail` |
| POST | `/templates` | A | `{"name","description","content"?,"from_builtin"?,"from_rule"?:{"rule_id","version"}}` | `TemplateDetail` |
| PUT | `/templates/{name}` | A | `{"description","content"}` | `TemplateDetail`（組み込みは 403） |
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

クエリ: `agent_id`、`category`（alert/normal）、`type`、`from`、`to`（最後の発生時刻で絞り込み）、`acknowledged`（true/false）、`suppressed`（true/false）、`limit`（既定 100、最大 500）、`offset`。

```ts
type DetectionType = 'exec' | 'denied' | 'read' | 'network' | 'writable' | 'exec_file';
interface Detection {
  id: number; agent_id: number; agent_name: string; hostname: string;
  type: DetectionType; category: 'alert' | 'normal';
  os_user: string; exe: string; target: string;
  count: number; first_at: string; last_at: string;
  file: { ctime: string; owner: string; mode: string } | null;
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
| POST | `/suppressions` | A | `{"agent_id"?,"type","os_user","exe","target","comment"}` | `Suppression` |
| DELETE | `/suppressions/{id}` | A | - | `{}` |

```ts
interface Suppression {
  id: number; agent_id: number | null; agent_name: string;   // null は全エージェント
  type: DetectionType | ''; os_user: string; exe: string; target: string;   // '' は任意。exe と target は * を使える
  comment: string; created_at: string; created_by: string;
}
```

## 10. 証明書

| GET | `/certificates` | V | - | `CertStatus` |
| --- | --- | --- | --- | --- |

```ts
interface CertInfo { fingerprint: string; subject: string; not_before: string; not_after: string; status: 'active' | 'next' | 'retired' }
interface CertStatus {
  cas: CertInfo[];
  server: CertInfo & { names: string[]; issuer_fingerprint: string };
  rotation: { state: 'none' | 'distributing' | 'switched'; next_ca_created_at: string | null; switch_after: string | null };
  agents_missing_new_ca: { agent_id: number; display_name: string }[];
}
```

## 11. 履歴

| GET | `/history?category=&limit=&offset=` | V | - | `{"items":HistoryItem[],"total":N}` |
| --- | --- | --- | --- | --- |

```ts
type HistoryCategory = 'account' | 'rule' | 'token' | 'certificate' | 'rejected' | 'template' | 'suppression' | 'detection';
interface HistoryItem {
  id: number; at: string; account_name: string;   // システムの処理は "system"
  category: HistoryCategory; action: string; target: string; detail: string;
  remote_addr: string; count: number;            // 拒否した接続は同じ接続元と理由をまとめて数える
}
```

## 12. アカウント

| メソッド | パス | 権限 | 要求 | 応答 |
| --- | --- | --- | --- | --- |
| GET | `/accounts` | A | - | `Account[]` |
| POST | `/accounts` | A | `{"name","role","language"?,"password"}` | `Account`（`language` は `auto` / `en` / `ja`、省略時は `auto`） |
| PUT | `/accounts/{id}/role` | A | `{"role"}` | `Account` |
| PUT | `/accounts/{id}/password` | A | `{"password"}` | `Account` |
| POST | `/accounts/{id}/disable` | A | - | `Account` |
| POST | `/accounts/{id}/enable` | A | - | `Account` |
| DELETE | `/accounts/{id}` | A | - | `{}` |

```ts
interface Account {
  id: number; name: string; role: 'admin' | 'viewer'; language: 'auto' | 'en' | 'ja'; disabled: boolean;
  locked_until: string | null; created_at: string;
}
```

- 有効な管理者が 1 人だけの場合、その管理者の削除、無効化、閲覧者への変更は 409 です。
- パスワードは 10 文字以上です。

## 13. 導入の案内

| GET | `/guide` | V | - | `Guide` |
| --- | --- | --- | --- | --- |

```ts
interface Guide {
  version: string; central_hostname: string;
  agent_urls: string[];          // エージェントが接続に使える URL（サーバー証明書の名前から作る）
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

- すべての認証付きの要求に `X-Mimamori-Instance-Id`（エージェントが初期化時に作ってデータディレクトリに保存するインスタンス ID）を付けます。トークンが無い、無効、期限切れ、失効、インスタンスIDの不一致の場合は、理由を返さずに 401 とし、履歴（`rejected`）に残します。
- 詳細は [システム仕様.md](システム仕様.md) の「エージェントとセントラルの通信」を参照してください。
