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
./mimamori central init-admin --name admin
./mimamori central start
```

Open `http://<central>:9080` in a browser, log in, create a monitoring rule and issue a token.

On each monitored host:

```bash
./mimamori agent init --central-host central.example.com
./mimamori agent start
```

## central

Commands for the central. Run them on the central host.

| Command | Description |
| --- | --- |
| `central start` | Start the central (browser screens, agent API, management API and the built-in agent). Stops with SIGTERM or SIGINT |
| `central stop [--timeout DURATION]` | Stop the running central and wait until it has stopped (default 120s) |
| `central status` | Show whether the central is running, its listen addresses and workers. Exit code 3 when it is not running |
| `central init-admin --name NAME [--language auto\|en\|ja]` | Create the first administrator (only while no administrator exists). The password is read from the terminal or the first line of standard input |
| `central reset-password --name NAME` | Reset an administrator's password, enable the account and invalidate its sessions |
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

Management commands. Run them on the central host while the central is running. `--json` prints the raw JSON.

### agents

| Command | Description |
| --- | --- |
| `ctl agents list` | List agents |
| `ctl agents show ID` | Show an agent with its host information |

### tokens

| Command | Description |
| --- | --- |
| `ctl tokens issue NAME [--valid-days N] [--rule ID] [--rule-version V]` | Issue a token and show its value |
| `ctl tokens list` | List tokens with their values in full |
| `ctl tokens show ID` | Show a token with its value in full |
| `ctl tokens reissue ID [--valid-days N]` | Reissue a token with the same name; the previous values stop working |
| `ctl tokens unbind ID` | Remove the binding to an agent instance |
| `ctl tokens revoke ID` | Revoke a token immediately |

### rules

| Command | Description |
| --- | --- |
| `ctl rules list` | List rules |
| `ctl rules show ID [--version V]` | Show a rule (latest or given version) |
| `ctl rules create NAME [--template T]... [--choice FIELD=TEMPLATE]... [--file F] [--description D]` | Create a rule from templates, from a JSON file (`-` for standard input) or empty |
| `ctl rules export ID [--version V]` | Print the content of a version as JSON |
| `ctl rules save ID --file F [--comment C] [--description D]` | Save edited content as a new version |
| `ctl rules edit ID [--comment C] [--editor CMD]` | Edit the latest version with an editor (default `vi`) and save it as a new version |
| `ctl rules history ID` | List the versions of a rule |
| `ctl rules diff ID FROM TO` | Show the difference between two versions |
| `ctl rules apply ID --agent ID... [--version V]` | Apply a version to agents |
| `ctl rules to-template ID NAME [--version V] [--description D]` | Define an administrator template from a version |
| `ctl rules delete ID` | Delete a rule (not possible while applied) |

### templates

| Command | Description |
| --- | --- |
| `ctl templates list` | List templates |
| `ctl templates show NAME` | Show a template |
| `ctl templates create NAME [--from-builtin T \| --from-rule ID --version V \| --file F] [--description D]` | Create an administrator template |
| `ctl templates update NAME --file F [--description D]` | Change an administrator template |
| `ctl templates delete NAME` | Delete an administrator template |

### detections

| Command | Description |
| --- | --- |
| `ctl detections list [--agent ID] [--category alert\|normal] [--type T] [--from T] [--to T] [--acknowledged true\|false] [--suppressed true\|false] [--limit N] [--offset N]` | List detections. `T` is RFC3339 or `YYYY-MM-DD[THH:MM[:SS]]` (local time); a value that cannot be parsed is an error |
| `ctl detections show AGENT_ID ID` | Show a detection |
| `ctl detections ack AGENT_ID:ID...` | Mark detections as acknowledged |

### suppressions

| Command | Description |
| --- | --- |
| `ctl suppressions list` | List suppression conditions |
| `ctl suppressions create [--agent ID] [--type T] [--user U] [--exe E] [--target T] [--comment C]` | Create a suppression condition (empty fields match anything; `--exe` and `--target` accept `*`) |
| `ctl suppressions delete ID` | Delete a suppression condition |

### accounts

| Command | Description |
| --- | --- |
| `ctl accounts list` | List accounts |
| `ctl accounts create NAME --role admin\|viewer [--language auto\|en\|ja]` | Create an account (password from standard input) |
| `ctl accounts role ID admin\|viewer` | Change the role |
| `ctl accounts password ID` | Reset the password (from standard input) |
| `ctl accounts disable ID` | Disable an account |
| `ctl accounts enable ID` | Enable an account |
| `ctl accounts delete ID` | Delete an account |

### certs and history

| Command | Description |
| --- | --- |
| `ctl certs status` | Show the CA and server certificate status |
| `ctl history [--category C] [--limit N] [--offset N]` | Show the history. Categories: `account`, `rule`, `token`, `certificate`, `rejected`, `template`, `suppression`, `detection`, `config` |

## version

| Command | Description |
| --- | --- |
| `version` | Show the version |

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Unspecified error |
| 2 | Invalid arguments or flags (also errors in the command line itself, such as an unknown command or flag or a missing value) |
| 3 | Configuration or database error, including another instance already running, a listen address that cannot be opened (for example in use) and `agent init` or `agent set-central` while the agent is running; for `status`, not running |
| 4 | Authentication or authorization failure |
| 5 | Execution error (external command or the central) |
| 6 | File or directory I/O error |
| 7 | Timeout |
