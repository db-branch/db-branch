# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to follow [Semantic Versioning](https://semver.org/) from 1.0. Until then (Beta), APIs may change.

## [Unreleased]

### Added

- Branch checkpoints. Save a running branch's state (a ZFS snapshot; instant, the server keeps running) and put the branch back to it later, from the console (a Checkpoints section in each branch card and in the admin Branches page), the CLI (`branch checkpoint`, `checkpoints`, `restore`, `checkpoint delete`), the API (`/v1/branches/{name}/checkpoints`) and MCP (`create_checkpoint`, `list_checkpoints`, `restore_checkpoint`). Restoring loses nothing: the current state is saved as a `before-restore-<time>` checkpoint, every other checkpoint stays restorable any number of times, and an interrupted restore is finished when the agent restarts. Reset keeps the checkpoints, deleting a branch removes them, and the new `branch.max_checkpoints` setting (default 20; the automatic checkpoint may exceed it by one) limits them. New actions `branch.checkpoint.read`, `branch.checkpoint`, `branch.restore` and `branch.checkpoint_delete` are for the owner and admins only. Needs the node's own ZFS (not module storage). See `docs/checkpoints.md`.
- Extension points for the paid `history` module (additive; existing modules are unaffected): `ext.ServerOptioner` adds lines to a branch server's configuration, `ext.RestoreFinisher` lets a module work on restored data while the server is in maintenance mode (no network), and `ext.Host.Branches` (`ext.BranchHost`) gives a module this node's branches: checkpoints and lines, restore (also of the creation state) with a note, the server's admin socket, read-only mounts of any line or checkpoint, and operations of its own. `/v1/ext/<module>/branches/<name>/...` is answered by the node holding the branch. New `license.FeatureHistory`. See `docs/modules.md`.
- `POST /v1/branches` takes `record` (CLI `branch create --record`, MCP `create_branch`), `GET /v1/capabilities` says whether recording is available (`record`), and the branch carries `record`. `GET /v1/branches/{name}/checkpoints` also returns the structure of the lines (`lines`, `live`, `head`), which the console draws as a history graph. A branch record gains the `record` field: in a cluster with older nodes still running, an older node that rewrites the record drops it, so upgrade every node before relying on it.
- Public-release documentation: README, SECURITY, SUPPORT, CODE_OF_CONDUCT, issue and pull request templates, Dependabot configuration, and an English index of `docs/`.

## [0.2.0] - TBD - first public release (Beta)

### Added

- Database branches on ZFS clones for MySQL (8.0, 8.4, 9.x) and PostgreSQL (17, 18), with several engine versions side by side (`db-branch runtime install|list|available|remove`). Each branch runs its own database server.
- Bases (loaded from a SQL dump, optionally gzip-compressed), snapshots, branch create, reset, recreate, extend and delete, TTL-based expiry, init SQL, and `branch dsn`.
- Scheduled base refresh from a source database (`base refresh`).
- Anonymous (no sign-in) branches from published snapshots, with cluster-wide and per-base limits and a per-client-address rate limit.
- Raft cluster of nodes (`cluster init|join|token|status`) with mutual-TLS between nodes, base replication to chosen nodes, cluster-wide runtime installation, and backup and restore of cluster state.
- REST API with an OpenAPI 3.1 definition, `db-branch` CLI, web console, and an MCP server for LLM agents with personal and service API tokens (`branches` or `full` scope).
- ProxySQL module: routing to branches by user name or by `/*db_branch='name'*/` query comment, and shared route users.
- Data masking modules (`greenmask`, `greenmask-mysql`, `anon`) and `base mask`, which records the masker, rules and source snapshot on the result.
- Extension points for modules (`pkg/ext`, `pkg/ext/plugin`): authentication, authorization, audit sink, router, settings, pages, API handlers and a cluster-shared store. Cluster-wide module install (`module install|remove|list`) and settings (`settings list|set`).
- Licence keys for paid modules, with monthly and yearly subscriptions and automatic renewal that can be turned off for air-gapped clusters.
- Prometheus metrics, health endpoints, and Slack and webhook notifications.
- Minor-version updates of the whole cluster from the console or `db-branch update`.
- Debian packages for `db-branch`, `db-branch-proxysql`, `db-branch-greenmask`, `db-branch-greenmask-mysql` and `db-branch-anon`, published in a signed apt repository at <https://apt.db-branch.com>.
- Support for Ubuntu 24.04 LTS and 26.04 LTS.
- Static browser-only demo console at <https://demo.db-branch.com/>.
