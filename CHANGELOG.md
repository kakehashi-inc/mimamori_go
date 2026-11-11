# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Central service (`mimamori central start`) that receives reports from agents, stores detections, sends email notifications and serves the browser screens and the management API.
- Agent (`mimamori agent init` / `mimamori agent start`) that monitors program execution, permission-denied file operations, reads of selected directories and network connections of selected executables through auditd, and scans for writable files outside allowed paths and for new or changed executable files inside them.
- Built-in agent that monitors the central host itself; when auditd is unavailable the central keeps running and shows that the built-in agent is stopped.
- Monitoring rules with versions, history and differences between versions, created from built-in templates (`base`, `apache`, `nginx`, `php-fpm`, `laravel`, `ffmpeg`), administrator templates or from scratch, and applied to selected agents.
- Agent tokens with expiry, machine binding, automatic renewal, reissue, unbinding and revocation.
- Private CA and server certificates created, renewed and rolled over automatically; agents receive new CA certificates over their existing trusted connection.
- Detection list with filters, acknowledgement and suppression conditions; email notifications with a minimum interval, repeat suppression and alert marking in the subject.
- Accounts with administrator and viewer roles, login lockout, CLI credentials, first-administrator creation and administrator password recovery on the central host.
- Browser screens in Japanese and English for agents, tokens, detections, rules, templates, suppressions, certificates, history, accounts, own settings and a setup guide.
- Management CLI (`mimamori ctl`) offering the same operations as the browser screens on the central host.
