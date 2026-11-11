# mimamori

[日本語](README-ja.md)

## 1. Overview

mimamori detects operations performed with the privileges of selected OS users on each host (for example the users that run the web server or php-fpm), using auditd and file scans, collects them on a central server and notifies by email. Administrators decide what to monitor and what is important (`alert`) with monitoring rules.

### Features

- Monitoring types: program execution, file operations denied by permissions, successful reads of selected directories, network connections started by selected executables, writable files and directories outside the allowed paths, and newly created or changed executable files (such as `.php`) inside the allowed paths
- Operations of logged-in users and cron jobs can be excluded, so that only processes started by the system are monitored
- Monitoring rules hold values only and are versioned. They are created by combining built-in templates (`base`, `apache`, `nginx`, `php-fpm`, `laravel`, `ffmpeg`) and administrator templates, and a saved version is applied to selected agents
- Agents authenticate with tokens (with an expiry, bound to the host's machine ID, renewed automatically) and connect over HTTPS with certificates from a private CA that the central creates automatically
- Detections are grouped by type, OS user, executable and target, and handled with suppression conditions, acknowledgement and notifications (through your own SMTP server) with a minimum interval and repeat suppression
- The same operations are available in the browser screens (Japanese / English) and in the CLI on the central host. Roles are administrator and viewer
- The central host itself is monitored by the built-in agent

### Architecture

| Component | Description |
| --- | --- |
| Executable | `mimamori` (a single Go binary containing the central, the agent and the CLI) |
| Central | `mimamori central start`. Two listeners: for browsers (HTTP, default `:8080`) and for agents and management (HTTPS with the private CA, default `:8443`) |
| Agent | `mimamori agent start`. Runs on each host with permission to control auditd (usually root, because `auditctl` requires it) and only connects to the central |
| Data | SQLite, split into the central management database `central.db` and one database per monitored target `targets/agent-<id>.db` |
| Flows | One flow for the agent; basically two for the central (central functions and the built-in agent). The maximum number of workers handling requests is configurable (default 1) |

### Requirements

- Supported OS: Amazon Linux 2023, Rocky Linux 9, AlmaLinux 9, Ubuntu 24.04 (x64)
- auditd installed and running on every host, including the central host
- Notifications use your own SMTP server. No external services or database servers are used

### Getting started

1. On the central host, prepare the configuration file and start the central. To use the built-in agent too, run it with permission to control auditd (usually root).

    ```bash
    sudo install -d -m 700 /etc/mimamori
    mimamori central example-config | sudo tee /etc/mimamori/central.json >/dev/null
    sudo chmod 600 /etc/mimamori/central.json
    sudo mimamori central init-admin --name admin
    sudo mimamori central start
    ```

    Set `server_names` to the host names and IP addresses agents use to connect. Run the process as a service (systemd and so on) as suits your environment.

2. Open `http://<central>:8080` in a browser, log in, create a monitoring rule and issue a token.
3. On each monitored host, initialize the agent with the token given on standard input, then start it.

    ```bash
    printf '%s\n' '<token>' | sudo mimamori agent init --server https://<central>:8443 --ca-fingerprint <private CA fingerprint>
    sudo mimamori agent start
    ```

    Stop them and check their state with `sudo mimamori central stop` / `sudo mimamori central status` and `sudo mimamori agent stop` / `sudo mimamori agent status` (under a supervisor, SIGTERM stops them).

4. Use the CLI on the central host when needed (issue a CLI credential on the accounts screen).

    ```bash
    export MIMAMORI_CLI_KEY=mmc_...
    sudo -E mimamori ctl agents list
    ```

The setup guide screen explains the auditd checks and settings required on each host.

### Documents

- [Documents/システム仕様.md](Documents/システム仕様.md): design (terms, flows, CLI, configuration, monitoring rules, communication, auditd handling, notifications, certificates, screens)
- [Documents/API仕様.md](Documents/API仕様.md): HTTP API
- [Documents/テーブル定義.md](Documents/テーブル定義.md): table definitions
- [CHANGELOG.md](CHANGELOG.md): change log

## 2. Developer reference

### Generating icons

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

### Go commands

Add or update the debugger

```bash
go install github.com/go-delve/delve/cmd/dlv@latest
```

Add a module

```bash
go get <package-name>
```

Add and build a module

```bash
go install <package-name>
```

Create the module file

```bash
go mod init <module-name>
```

Download modules (all of go.mod when the module name is omitted)

```bash
go mod download <module-name>
```

Tidy modules (keep the sources and go.mod in sync)

```bash
go mod tidy
```

Update modules to the latest versions

```bash
go get -u
```

Update the Go version

```bash
go mod tidy --go=1.25
```

Clear the caches

```bash
go clean --cache --testcache
```

### Frontend commands

Install dependencies

```bash
cd frontend
yarn install
```

Build (output to `frontend/dist`; required before `go build` or `go vet` because Go's `go:embed` refers to it)

```bash
cd frontend
yarn build
```

Type check

```bash
cd frontend
yarn lint
```

Format (under `src`)

```bash
cd frontend
yarn format
```

Place the Material Icons fonts (copy from `node_modules` to `public/fonts`)

```bash
cd frontend
yarn setup:fonts
```

### Start

```bash
go run . service start
```

The web screens open at http://localhost:8080 (the API server is on 8081; `service stop` / `status` connect to the API port). Runtime data (such as the database file) is created in `data/` next to the executable.

### Lint and test

```bash
make lint
make test
```

### Build and release

Build (`cd frontend && yarn build` is required first)

```bash
go build
```

Release

```bash
make
```
