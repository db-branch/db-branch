# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to follow [Semantic Versioning](https://semver.org/) from 1.0. Until then (Beta), APIs may change.

## [Unreleased]

### Added

- Public-release documentation: README, SECURITY, SUPPORT, CODE_OF_CONDUCT, issue and pull request templates, Dependabot configuration, and an English index of `docs/`.

## [0.2.0] - TBD - first public release (Beta)

### Added

- Database branches on ZFS clones for MySQL (8.0, 8.4, 9.x) and PostgreSQL (17, 18), with several engine versions side by side (`db-branch runtime install|list|available|remove`). Each branch runs its own database server.
- Bases (loaded from a SQL dump, optionally gzip-compressed), snapshots, branch create, reset, recreate, extend and delete, TTL-based expiry, init SQL, and `branch dsn`.
- Scheduled base refresh from a source database (`base refresh`).
- Anonymous (no sign-in) branches from published snapshots, with global, per-base and per-source limits.
- Raft cluster of nodes (`cluster init|join|token|status`) with mutual-TLS between nodes, base replication to chosen nodes, cluster-wide runtime installation, and backup and restore of cluster state.
- REST API with an OpenAPI 3.1 definition, `db-branch` CLI, web console, and an MCP server for LLM agents with personal and service API tokens (`branches` or `full` scope).
- ProxySQL module: routing to branches by user name or by `/*db_branch='name'*/` query comment, and shared route users.
- Data masking modules (`greenmask`, `greenmask-mysql`, `anon`) and `base mask`, which records the masker, rules and source snapshot on the result.
- Extension points for modules (`pkg/ext`, `pkg/ext/plugin`): authentication, authorization, audit sink, router, settings, pages, API handlers and a cluster-shared store. Cluster-wide module install (`module install|remove|list`) and settings (`settings list|set`).
- Licence keys for paid modules, with monthly, yearly and one-time keys and automatic renewal that can be turned off for air-gapped clusters.
- Prometheus metrics, health endpoints, and Slack and webhook notifications.
- Minor-version updates of the whole cluster from the console or `db-branch update`.
- Debian packages for `db-branch`, `db-branch-proxysql`, `db-branch-greenmask`, `db-branch-greenmask-mysql` and `db-branch-anon`, published in a signed apt repository at <https://apt.db-branch.com>.
- Support for Ubuntu 24.04 LTS and 26.04 LTS.
- Static browser-only demo console at <https://demo.db-branch.com/>.
