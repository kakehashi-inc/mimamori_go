# mimamori command reference

[日本語](CommandReference-ja.md)

```text
mimamori [--data-dir DIR] <command> [arguments] [flags]
```

- `--data-dir DIR`: data directory. Default: `data/` next to the executable.
- `mimamori <command> --help` shows the help of each command.

## Getting started

On the central host:

```bash
./mimamori central config set server_names central.example.com
./mimamori central init-admin --email admin@example.com --display-name Admin
./mimamori central start
```

Open `https://<central>:9580` in a browser, log in, create a monitoring rule and issue a token.

On each monitored host:

```bash
./mimamori agent init --central-host central.example.com
./mimamori agent start
```

`agent init` asks for the value of the token. `central start` and `agent start` run in the foreground until they receive SIGTERM or SIGINT.

## central

Commands for the central. Run them on the central host.

| Command | Description |
| --- | --- |
| `central start` | Start the central |
| `central stop [--timeout DURATION]` | Stop the running central and wait until it has stopped. `--timeout` defaults to 120s |
| `central status` | Show whether the central is running, the data directory and the listen addresses |
| `central init-admin --email ADDRESS --display-name NAME [--language auto\|en\|ja]` | Create the first administrator. The password is read from the terminal or standard input. `--language` defaults to `auto` |
| `central reset-password --email ADDRESS` | Reset the password of an administrator. The password is read from the terminal or standard input |
| `central config list` | List every configuration item |
| `central config get NAME` | Print the value of one item |
| `central config set NAME [VALUE]` | Set one item. Lists are comma separated. `smtp.password` is read from the terminal or standard input |
| `central config unset NAME` | Reset one item to its default |

## agent

Commands for the agent. Run them on each monitored host.

| Command | Description |
| --- | --- |
| `agent init --central-host HOST [--central-port PORT] [--overwrite] [--ca-fingerprint FP]` | Initialize the agent. The token is read from the terminal or standard input. `--central-port` defaults to 9581. `--overwrite` re-initializes an initialized agent. `--ca-fingerprint` checks the fingerprint of the central's CA |
| `agent set-central --host HOST [--port PORT]` | Change the central the agent connects to. Without `--port` the current port is kept |
| `agent set-workers N [--builtin]` | Set the worker count of the agent (1-64, default 1). With `--builtin`, set the count of the built-in agent on the central host. Takes effect at the next start |
| `agent start` | Start the agent |
| `agent stop [--timeout DURATION]` | Stop the running agent and wait until it has stopped. `--timeout` defaults to 300s |
| `agent status` | Show whether the agent is running, the central, the token, the worker count, the applied rule and the unsent reports |

## ctl

Management commands. Run them on the central host while the central is running, as a user who can read the data directory. `--json` prints JSON.

### agents

| Command | Description |
| --- | --- |
| `ctl agents list` | List agents |
| `ctl agents show ID` | Show an agent with its host information |

### tokens

| Command | Description |
| --- | --- |
| `ctl tokens issue NAME [--valid-days N] [--rule ID] [--rule-version V]` | Issue a token and show the `agent init` command to run on the monitored host. `--valid-days` defaults to the `token_valid_days` setting; `--rule-version` defaults to the latest version |
| `ctl tokens list` | List tokens |
| `ctl tokens show ID` | Show a token |
| `ctl tokens reissue ID [--valid-days N]` | Reissue a token with the same name. The previous value stops working |
| `ctl tokens unbind ID` | Remove the binding to an agent |
| `ctl tokens revoke ID` | Revoke a token |

### rules

| Command | Description |
| --- | --- |
| `ctl rules list` | List rules |
| `ctl rules show ID [--version V]` | Show a rule. `--version` defaults to the latest version |
| `ctl rules create NAME [--template T]... [--choice FIELD=TEMPLATE]... [--file F] [--description D]` | Create a rule from templates, from a JSON file (`-` for standard input) or empty |
| `ctl rules export ID [--version V]` | Print the content of a version as JSON |
| `ctl rules save ID --file F [--comment C] [--description D]` | Save the content of a file as a new version |
| `ctl rules edit ID [--comment C] [--editor CMD]` | Edit the latest version with an editor (default `vi`) and save it as a new version |
| `ctl rules history ID` | List the versions |
| `ctl rules diff ID FROM TO` | Show the difference between two versions |
| `ctl rules apply ID --agent ID... [--version V]` | Apply a version to agents. `--version` defaults to the latest version |
| `ctl rules unassign --agent ID...` | Take the rule away from agents |
| `ctl rules to-template ID NAME [--version V] [--description D]` | Create a template from a version |
| `ctl rules delete ID` | Delete a rule |

### templates

| Command | Description |
| --- | --- |
| `ctl templates list` | List templates |
| `ctl templates show NAME` | Show a template |
| `ctl templates create NAME [--from-builtin T \| --from-rule ID [--version V] \| --file F] [--description D]` | Create a template from a built-in template, a version of a rule or a file |
| `ctl templates update NAME --file F [--description D]` | Change a template created by an administrator |
| `ctl templates delete NAME` | Delete a template created by an administrator |

### detections

| Command | Description |
| --- | --- |
| `ctl detections list [--agent ID] [--category alert\|normal] [--type exec\|denied\|read\|network\|writable\|exec_file] [--from T] [--to T] [--acknowledged true\|false] [--suppressed true\|false] [--limit N] [--offset N]` | List detections. `T` is `YYYY-MM-DD HH:MM:SS`, `YYYY-MM-DD` and similar in local time, or RFC3339. `--limit` defaults to 100 (at most 500) |
| `ctl detections show AGENT_ID ID` | Show a detection |
| `ctl detections ack AGENT_ID:ID...` | Mark detections as acknowledged |

### suppressions

| Command | Description |
| --- | --- |
| `ctl suppressions list` | List suppression conditions |
| `ctl suppressions create [--agent ID] [--type T] [--user U] [--exe E] [--target T] [--comment C]` | Create a suppression condition. Without `--agent` it applies to all agents. `--exe` and `--target` accept `*` |
| `ctl suppressions delete ID` | Delete a suppression condition |

### command-sets

`AGENT_ID` is the id of `ctl agents list`.

| Command | Description |
| --- | --- |
| `ctl command-sets list AGENT_ID` | List the command sets of an agent |
| `ctl command-sets show AGENT_ID SET_ID` | Show a command set |
| `ctl command-sets export AGENT_ID SET_ID` | Print the definition of a command set as JSON |
| `ctl command-sets create AGENT_ID --file F` | Create a command set from a definition file (`-` for standard input) |
| `ctl command-sets update AGENT_ID SET_ID --file F` | Replace the definition of a command set with a definition file |
| `ctl command-sets delete AGENT_ID SET_ID` | Delete a command set |
| `ctl command-sets import AGENT_ID --from SOURCE_AGENT_ID SET_ID[=NEW_NAME]...` | Copy command sets from another agent. `=NEW_NAME` renames the copy |
| `ctl command-sets run AGENT_ID SET_ID` | Request a run of a command set |
| `ctl command-sets discard AGENT_ID REQUEST_ID` | Discard a run request not yet handed to the agent |
| `ctl command-sets runs AGENT_ID SET_ID [--limit N] [--before RUN_ID]` | List the runs, newest first. `--limit` defaults to 50 |
| `ctl command-sets show-run AGENT_ID RUN_ID` | Show the result of a run |

The definition file is one JSON object (see [API仕様.md](API仕様.md), chapter 4.1, for its fields). Example:

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

| Command | Description |
| --- | --- |
| `ctl accounts list` | List accounts |
| `ctl accounts create EMAIL --display-name NAME --role admin\|viewer [--language auto\|en\|ja]` | Create an account. The password is read from standard input. `--language` defaults to `auto` |
| `ctl accounts email ID ADDRESS` | Change the e-mail address |
| `ctl accounts display-name ID NAME` | Change the display name |
| `ctl accounts role ID admin\|viewer` | Change the role |
| `ctl accounts notify ID on\|off` | Choose whether the account receives notification mails |
| `ctl accounts password ID` | Reset the password. The password is read from standard input |
| `ctl accounts disable ID` | Disable an account |
| `ctl accounts enable ID` | Enable an account |
| `ctl accounts delete ID` | Delete an account |

### certs and history

| Command | Description |
| --- | --- |
| `ctl certs status` | Show the status of the private CA and the server certificate |
| `ctl history [--category C] [--limit N] [--offset N]` | Show the history. `C` is one of `account`, `rule`, `token`, `certificate`, `rejected`, `template`, `suppression`, `detection`, `config`, `command`. `--limit` defaults to 50 |

## version

| Command | Description |
| --- | --- |
| `version` | Show the version |

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Unspecified error |
| 2 | Invalid arguments |
| 3 | Configuration or database error; for `status`, not running |
| 4 | Authentication or authorization failure |
| 5 | Execution error (external command or the central) |
| 6 | File or directory I/O error |
| 7 | Timeout (`stop` waited longer than `--timeout`) |
