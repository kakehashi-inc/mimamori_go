# mimamori

[日本語](README-ja.md)

## 1. Overview

mimamori detects operations performed with the privileges of selected OS users on each host, using auditd and file scans, collects them on a central server and notifies by email. Administrators decide what to monitor and what is important with monitoring rules.

### Features

- Detection of program execution, file operations denied by permissions, reads of selected directories, network connections by selected executables, writable files and newly created executable files
- Monitoring rules built from templates, with versions, applied to agents
- Acknowledgement of detections, suppression conditions and email notifications
- Management on browser screens (English / Japanese) and with a CLI on the central host
- Monitoring of the central host itself (built-in agent)

### Requirements

- Supported OS: Amazon Linux 2023, Rocky Linux 9, AlmaLinux 9, Ubuntu 24.04 (x64)
- auditd running on each host (including the central host)
- An SMTP server for notifications

### Getting started

1. On the central host, set the configuration and start the central.

    ```bash
    ./mimamori central config set server_names central.example.com
    ./mimamori central init-admin --name admin
    ./mimamori central start
    ```

2. Open `http://<central>:9080` in a browser, log in, create a monitoring rule and issue a token.
3. On each monitored host, initialize and start the agent.

    ```bash
    ./mimamori agent init --server https://<central>:9081
    ./mimamori agent start
    ```

The auditd checks and settings each host needs are shown on the "Setup guide" screen.

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
