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
| `central status` | Show whether the central is running, its listen addresses and workers. Exit code 3 when it is not running |
| `central init-admin --email ADDRESS --display-name NAME [--language auto\|en\|ja]` | Create the first administrator. `ADDRESS` is the e-mail address used to log in; `NAME` is the name shown on the screens. `--language` defaults to `auto`. Only while no administrator exists. The password is read from the terminal or the first line of standard input |
| `central reset-password --email ADDRESS` | Reset the password of the administrator with this e-mail address (case is ignored), enable the account and invalidate its sessions, trusted browsers and pending verification codes |
| `central config list` | List every configuration item with its value, whether it is set or the default, and when a change takes effect |
| `central config get NAME` | Print the value of one item |
| `central config set NAME [VALUE]` | Set one item. Lists are comma separated. `smtp.password` is read from the terminal or standard input |
| `central config unset NAME` | Remove one item so its default applies again |

A running central applies a change made with `central config` at its next periodic check (every 30 seconds); a change made on the configuration screen is applied at once. `web_listen`, `agent_listen`, `workers`, `queue_timeout_sec` and `builtin_agent` take effect when the central restarts.

## agent

Commands for the agent. Run them on each monitored host.

| Command | Description |
| --- | --- |
| `agent init --central-host HOST [--central-port PORT] [--overwrite] [--ca-fingerprint FP]` | Initialize the agent. `HOST` is the host name or IP address of the central; `PORT` defaults to 9081. The token is read from the terminal (not echoed) or the first line of standard input. `--overwrite` re-initializes an initialized agent (only while the agent is stopped) |
| `agent set-central --host HOST [--port PORT]` | Change the central host (and port) of the initialized agent. Only while the agent is stopped; saved only when the agent can connect to the new address with its current token |
| `agent start` | Start the agent. Stops with SIGTERM or SIGINT |
| `agent stop [--timeout DURATION]` | Stop the running agent and wait until it has finished its cycle (default 300s) |
| `agent status` | Show whether the agent is running, the central, the token, the applied rule and unsent reports. Exit code 3 when it is not running |

## ctl

Management commands. Run them on the central host while the central is running. `--json` prints the raw JSON (`ctl rules export` always prints JSON, and `ctl rules edit` prints only its messages). Tables show times in local time and print other values as they are; long values are shortened to 60 characters (except in `ctl tokens list`). A content file given with `--file` must contain a JSON object. A flag that does not apply to the other flags given is ignored without an error; the notes of each command below say which one is used.

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
| `ctl detections ack AGENT_ID:ID...` | Mark detections as acknowledged. `AGENT_ID` and `ID` must be integers |

### suppressions

| Command | Description |
| --- | --- |
| `ctl suppressions list` | List suppression conditions |
| `ctl suppressions create [--agent ID] [--type T] [--user U] [--exe E] [--target T] [--comment C]` | Create a suppression condition (empty fields match anything; `--exe` and `--target` accept `*`). At least one of `--type`, `--user`, `--exe` and `--target` is required; `--agent` alone is an argument error (without `--agent` the condition applies to all agents) |
| `ctl suppressions delete ID` | Delete a suppression condition |

### accounts

| Command | Description |
| --- | --- |
| `ctl accounts list` | List accounts (id, email, display_name, role, language, disabled, locked_until, created_at) |
| `ctl accounts create EMAIL --display-name NAME --role admin\|viewer [--language auto\|en\|ja]` | Create an account. `EMAIL` is the e-mail address used to log in (unique, case is ignored); `NAME` is the name shown on the screens (need not be unique). `--language` defaults to `auto`; password from standard input |
| `ctl accounts email ID ADDRESS` | Change the e-mail address at once, without a verification code. The account's sessions, trusted browsers and pending verification codes are invalidated |
| `ctl accounts display-name ID NAME` | Change the display name (sessions are kept) |
| `ctl accounts role ID admin\|viewer` | Change the role (the account's sessions are invalidated) |
| `ctl accounts password ID` | Reset the password (from standard input). The account's sessions, trusted browsers and pending verification codes are invalidated |
| `ctl accounts disable ID` | Disable an account. Its sessions, trusted browsers and pending verification codes are invalidated |
| `ctl accounts enable ID` | Enable an account |
| `ctl accounts delete ID` | Delete an account |

### certs and history

| Command | Description |
| --- | --- |
| `ctl certs status` | Show the CA and server certificate status |
| `ctl history [--category C] [--limit N] [--offset N]` | Show the history (`--limit` defaults to 50 and is held between 1 and 500; `--offset` defaults to 0). Categories: `account`, `rule`, `token`, `certificate`, `rejected`, `template`, `suppression`, `detection`, `config` |

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
