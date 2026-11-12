# mimamori command reference

[日本語](CommandReference-ja.md)

```text
mimamori [--data-dir DIR] <command> [arguments] [flags]
```

- `--data-dir DIR`: data directory. Default: `data/` next to the executable (`data/` in the current directory when the executable's directory cannot be determined). An empty value is an argument error (exit code 2).
- `mimamori <command> --help` shows the help of each command.

## Getting started

On the central host:

```bash
./mimamori central config set server_names central.example.com
./mimamori central init-admin --email admin@example.com --display-name Admin
./mimamori central start
```

`central start` runs in the foreground until it receives SIGTERM or SIGINT, so run it from a service manager or keep its terminal open. The `ctl` commands run in another terminal.

Open `http://<central>:9080` in a browser, log in, create a monitoring rule and issue a token. The same can be done with `ctl rules create` and `ctl tokens issue`.

On each monitored host:

```bash
./mimamori agent init --central-host central.example.com
./mimamori agent start
```

`agent init` asks for the token (the value of the issued token). `agent start` runs in the foreground until it receives SIGTERM or SIGINT.

## central

Commands for the central. Run them on the central host.

| Command | Description |
| --- | --- |
| `central start` | Start the central (browser screens, agent API, management API and the built-in agent). Stops with SIGTERM or SIGINT |
| `central stop [--timeout DURATION]` | Stop the running central and wait until it has stopped (default 120s) |
| `central status` | Show whether the central is running, its listen addresses and workers. While it runs, the values it started with are shown, and a saved value that differs is marked, for example `http://0.0.0.0:9080 (saved: 0.0.0.0:9090, takes effect after restart)`. When it is not running, the saved values are shown. Exit code 3 when it is not running |
| `central init-admin --email ADDRESS --display-name NAME [--language auto\|en\|ja]` | Create the first administrator. `ADDRESS` is the e-mail address used to log in; `NAME` is the name shown on the screens. `--language` defaults to `auto`. Only while no administrator exists. The password is read from the terminal or the first line of standard input |
| `central reset-password --email ADDRESS` | Reset the password of the administrator with this e-mail address (case is ignored), enable the account and invalidate its sessions, trusted browsers and pending verification codes |
| `central config list` | List every configuration item with its value, whether it is set or the default, and when a change takes effect |
| `central config get NAME` | Print the value of one item |
| `central config set NAME [VALUE]` | Set one item. Lists are comma separated. `smtp.password` is read from the terminal or standard input |
| `central config unset NAME` | Remove one item so its default applies again |

A running central applies a change made with `central config` at its next periodic check (every 30 seconds); a change made on the configuration screen is applied at once. `web_listen`, `agent_listen`, `workers`, `queue_timeout_sec` and `builtin_agent` take effect when the central restarts.

While `smtp.host` is set, logging in on the browser screens needs a verification code sent by e-mail. When nobody can log in because the code mail cannot be sent, fix the SMTP settings with `central config set smtp.*`, or stop asking for codes with `central config unset smtp.host` (both work while the central is stopped). Fix a wrong account address with `ctl accounts email ID ADDRESS` and a password with `central reset-password --email ADDRESS`.

## agent

Commands for the agent. Run them on each monitored host.

| Command | Description |
| --- | --- |
| `agent init --central-host HOST [--central-port PORT] [--overwrite] [--ca-fingerprint FP]` | Initialize the agent. `HOST` is the host name or IP address of the central; `PORT` defaults to 9081. The token is read from the terminal (not echoed) or the first line of standard input. `--overwrite` re-initializes an initialized agent (only while the agent is stopped) |
| `agent set-central --host HOST [--port PORT]` | Change the central host (and port) of the initialized agent. Only while the agent is stopped; saved only when the agent can connect to the new address with its current token. Without `--port` the current port is kept |
| `agent start` | Start the agent. Stops with SIGTERM or SIGINT |
| `agent stop [--timeout DURATION]` | Stop the running agent and wait until it has finished its cycle (default 300s) |
| `agent status` | Show whether the agent is running, the central, the token, the applied rule and unsent reports. Exit code 3 when it is not running |

## ctl

Management commands. Run them on the central host while the central is running. They connect to the address of `agent_listen` (to `127.0.0.1` when it listens on every address) and its port, and verify the central's certificate with `ca.crt` of the data directory; the management API answers only connections from the central host itself. `--json` prints the raw JSON (`ctl rules export` and `ctl command-sets export` always print JSON, and `ctl rules edit` prints only its messages). Tables show times in local time and print other values as they are; long values are shortened to 60 characters (except in `ctl tokens list`). A content file given with `--file` must contain a JSON object. A flag that does not apply to the other flags given is ignored without an error; the notes of each command below say which one is used.

### agents

| Command | Description |
| --- | --- |
| `ctl agents list` | List agents |
| `ctl agents show ID` | Show an agent with its host information |

### tokens

| Command | Description |
| --- | --- |
| `ctl tokens issue NAME [--valid-days N] [--rule ID] [--rule-version V]` | Issue a token (`--valid-days` defaults to the `token_valid_days` setting of the central, also when 0 or less is given; more than 3650 is an argument error; `--rule-version` applies only together with `--rule` and defaults to the latest version) and show its value with the `agent init` command to run on the monitored host (with `--central-port` when `agent_listen` does not use port 9081) |
| `ctl tokens list` | List tokens with their values in full |
| `ctl tokens show ID` | Show a token with its value in full |
| `ctl tokens reissue ID [--valid-days N]` | Reissue a token with the same name (`--valid-days` as in `tokens issue`); the previous values stop working. The `agent init` command shown includes `--overwrite` |
| `ctl tokens unbind ID` | Remove the binding to an agent instance |
| `ctl tokens revoke ID` | Revoke a token immediately |

### rules

| Command | Description |
| --- | --- |
| `ctl rules list` | List rules |
| `ctl rules show ID [--version V]` | Show a rule (given version; the latest when omitted or 0 or less) |
| `ctl rules create NAME [--template T]... [--choice FIELD=TEMPLATE]... [--file F] [--description D]` | Create a rule from templates, from a JSON file (`-` for standard input) or empty. Each `--choice` is one value (`FIELD` may contain commas). With `--template`, `--file` is ignored; `--choice` is used only together with `--template` |
| `ctl rules export ID [--version V]` | Print the content of a version as JSON (the latest version when `--version` is omitted) |
| `ctl rules save ID --file F [--comment C] [--description D]` | Save edited content as a new version |
| `ctl rules edit ID [--comment C] [--editor CMD]` | Edit the latest version with an editor (default `vi`) and save it as a new version |
| `ctl rules history ID` | List the versions of a rule |
| `ctl rules diff ID FROM TO` | Show the difference between two versions |
| `ctl rules apply ID --agent ID... [--version V]` | Apply a version (default: latest) to agents (an agent given more than once counts once; an agent that does not exist is an argument error) |
| `ctl rules to-template ID NAME [--version V] [--description D]` | Define an administrator template from a version (default: latest) |
| `ctl rules delete ID` | Delete a rule (not possible while applied) |

### templates

| Command | Description |
| --- | --- |
| `ctl templates list` | List templates |
| `ctl templates show NAME` | Show a template |
| `ctl templates create NAME [--from-builtin T \| --from-rule ID [--version V] \| --file F] [--description D]` | Create an administrator template (`--from-rule` copies the latest version unless `--version` is given; `--version` is used only with `--from-rule`). When more than one source is given, the first of `--from-builtin`, `--from-rule` and `--file` in this order is used and the others are ignored |
| `ctl templates update NAME --file F [--description D]` | Change an administrator template (the description is kept when `--description` is omitted) |
| `ctl templates delete NAME` | Delete an administrator template |

### detections

| Command | Description |
| --- | --- |
| `ctl detections list [--agent ID] [--category alert\|normal] [--type T] [--from T] [--to T] [--acknowledged true\|false] [--suppressed true\|false] [--limit N] [--offset N]` | List detections. `T` is `YYYY-MM-DD HH:MM:SS` (the form the tables show), `YYYY-MM-DD HH:MM`, `YYYY-MM-DDTHH:MM:SS`, `YYYY-MM-DDTHH:MM` or `YYYY-MM-DD` in local time, or RFC3339; any other value is an argument error. `--agent` 0 or less means all agents. `--limit` defaults to 100 and is held between 1 and 500 (0 gives 1 row, more than 500 gives 500); `--offset` defaults to 0 |
| `ctl detections show AGENT_ID ID` | Show a detection |
| `ctl detections ack AGENT_ID:ID...` | Mark detections as acknowledged. `AGENT_ID` and `ID` must be positive integers written with digits only, with nothing after them. When any of the given detections does not exist, nothing is acknowledged, the missing ones are named, and the exit code is 2 (as for any object that does not exist). Detections that are already acknowledged count as existing |

### suppressions

| Command | Description |
| --- | --- |
| `ctl suppressions list` | List suppression conditions |
| `ctl suppressions create [--agent ID] [--type T] [--user U] [--exe E] [--target T] [--comment C]` | Create a suppression condition (empty fields match anything; `--exe` and `--target` accept `*`). At least one of `--type`, `--user`, `--exe` and `--target` is required; `--agent` alone is an argument error (without `--agent` the condition applies to all agents) |
| `ctl suppressions delete ID` | Delete a suppression condition |

### command-sets

Command sets of one agent (the built-in agent included): definitions, run requests and results. `AGENT_ID` is the id of `ctl agents list`. These commands are never asked for a verification code.

| Command | Description |
| --- | --- |
| `ctl command-sets list AGENT_ID` | List the command sets of an agent. Above the table: the agent, the OS user new sets default to (the user the agent runs as; `(not reported yet)` before the agent has connected), the host's time zone, and whether the agent holds the current definitions |
| `ctl command-sets show AGENT_ID SET_ID` | Show a command set: its definition, schedule summary, open run request, last finished run, whether it is running, and problems the agent reported about it |
| `ctl command-sets export AGENT_ID SET_ID` | Print only the definition of a command set as JSON (always JSON), in the shape `create` and `update` read. The time of a once schedule is printed in local time with its offset |
| `ctl command-sets create AGENT_ID --file F` | Create a command set from a definition file (`-` for standard input). When `os_user` is omitted, the OS user the agent runs as is used (an agent that has never connected has none; then `os_user` is required) |
| `ctl command-sets update AGENT_ID SET_ID --file F` | Replace the whole definition of a command set with a definition file. When `os_user` is omitted, the set's current OS user is kept |
| `ctl command-sets delete AGENT_ID SET_ID` | Delete a command set. Its runs are kept until the retention removes them; a run request not yet delivered is discarded |
| `ctl command-sets import AGENT_ID --from SOURCE_AGENT_ID SET_ID[=NEW_NAME]...` | Copy command sets of the agent `SOURCE_AGENT_ID` (it may be `AGENT_ID` itself) into `AGENT_ID`, with everything including the OS user and the working directory. `=NEW_NAME` renames the copy (default: the source name; everything after the first `=` is the name). When a name is already used in the destination or repeated in the import, nothing is created and the names are printed. `SET_ID` must be a positive integer and `NEW_NAME` must not be empty |
| `ctl command-sets run AGENT_ID SET_ID` | Request a run of a command set (also of a scheduled one); the agent receives the request with its next sync. A set can have only one request that the agent has not answered; a request not delivered within 24 hours expires |
| `ctl command-sets discard AGENT_ID REQUEST_ID` | Discard a run request that was not yet delivered to the agent (`REQUEST_ID` is the `id` printed by `run`, or `request.id` of `show`) |
| `ctl command-sets runs AGENT_ID SET_ID [--limit N] [--before RUN_ID]` | List the runs of a command set, newest first (also of a deleted set while its runs are kept): trigger, due time of a scheduled run, start, end, state, reason and the display value on one line. `--limit` defaults to 50 and is held between 1 and 500; when older runs exist, the `--before` value of the next page is printed below the table |
| `ctl command-sets show-run AGENT_ID RUN_ID` | Show a run: trigger, OS user, working directory, times, state, display value, and for every executed command its result, exit code, times, command line, note and console output (up to 1 MiB per command), and how many commands were not executed |

The definition file is one JSON object in the shape of `CommandSetSpec` of the API (see [API仕様.md](API仕様.md), chapter 4.1, for every field, its values and limits); unknown fields are refused. Omitted fields take their defaults: run mode from whether a schedule is given, 100 kept runs, comma joiner, success code 0, failure codes 1 or more, `on_problems` `stop`, 300 seconds, 20 lines. Example:

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

- An empty `work_dir` is the home directory of the OS user. Every command line is run by bash exactly as written, as the set's OS user.
- Recurring schedules (`weekly`, `daily`, `every_hours`, `every_minutes`, `hourly_at`) run in the local time of the monitored host. The time of a once schedule (`{"kind": "once", "at": "2026-12-01 03:00"}`) may be written without an offset (`YYYY-MM-DD HH:MM[:SS]`, also with `T` instead of the space); it is then read in the local time of the host where `ctl` runs. A time in the past is refused.
- Values the central refuses (an invalid definition, a name already used by another set of the agent, a second open run request) end with exit code 2 and a message that names the field and, for a command, its position (from 1).

### accounts

| Command | Description |
| --- | --- |
| `ctl accounts list` | List accounts (id, email, display_name, role, language, disabled, notify, locked_until, created_at) |
| `ctl accounts create EMAIL --display-name NAME --role admin\|viewer [--language auto\|en\|ja]` | Create an account. `EMAIL` is the e-mail address used to log in (unique, case is ignored); `NAME` is the name shown on the screens (need not be unique). `--language` defaults to `auto`; password from standard input |
| `ctl accounts email ID ADDRESS` | Change the e-mail address at once, without a verification code. The account's sessions, trusted browsers and pending verification codes are invalidated |
| `ctl accounts display-name ID NAME` | Change the display name (sessions are kept) |
| `ctl accounts role ID admin\|viewer` | Change the role (the account's sessions are invalidated) |
| `ctl accounts notify ID on\|off` | Choose whether the account receives notification mails (detection notifications). New accounts are `off`. A disabled account keeps its choice but receives nothing while disabled. Sessions are kept |
| `ctl accounts password ID` | Reset the password (from standard input). The account's sessions, trusted browsers and pending verification codes are invalidated |
| `ctl accounts disable ID` | Disable an account. Its sessions, trusted browsers and pending verification codes are invalidated |
| `ctl accounts enable ID` | Enable an account |
| `ctl accounts delete ID` | Delete an account |

### certs and history

| Command | Description |
| --- | --- |
| `ctl certs status` | Show the CA and server certificate status |
| `ctl history [--category C] [--limit N] [--offset N]` | Show the history (`--limit` defaults to 50 and is held between 1 and 500; `--offset` defaults to 0). Categories: `account`, `rule`, `token`, `certificate`, `rejected`, `template`, `suppression`, `detection`, `config`, `command` |

## version

| Command | Description |
| --- | --- |
| `version` | Show the version |

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Unspecified error |
| 2 | Invalid arguments or flags (also errors in the command line itself, such as an unknown command or flag or a missing value, and values the central refuses, such as a duplicate name or a change to a built-in template) |
| 3 | Configuration or database error, including another instance already running, a listen address that cannot be opened (for example in use) and `agent init` or `agent set-central` while the agent is running; for `status`, not running |
| 4 | Authentication or authorization failure (for `ctl`, `cli.secret` that does not match the running central, or a central certificate that `ca.crt` cannot verify) |
| 5 | Execution error (external command or the central) |
| 6 | File or directory I/O error (for `ctl`, also `cli.secret` or `ca.crt` that cannot be read, or a `ca.crt` without a certificate) |
| 7 | Timeout |
