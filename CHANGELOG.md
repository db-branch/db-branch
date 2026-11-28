# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows [Semantic Versioning](https://semver.org/) from 1.0.

## [Unreleased]

### Added

- **Console & CLI `0.x → 1.x` updates (`major0`)**: clusters on `0.x` can now apply `1.x` releases from Settings → Updates, `db-branch update`, and `POST /v1/updates`, upgrading `0.x` modules to `1.x` alongside the core (while keeping `1.x → 2.x` and `0.x → 2.x` manual) and displaying a confirmation warning to read the release upgrade notes.

## [0.4.1] - 2026-11-28

PostgreSQL 19 support and PostgreSQL major version upgrades for branches, plus authentication hardening and dependency cleanups.

### Upgrade notes (read before upgrading)

- **Raft Protocol `30` → `32`**: upgrade nodes one at a time, followers first and the leader last (Settings → Updates in the console, `db-branch update apply 0.4.1`, or `apt update && apt upgrade` on each node). Until every node runs `0.4.1`, PostgreSQL major upgrades of branches and the new password hash format are not used. Do not downgrade a node after the cluster has moved to protocol `32`: passwords re-hashed on login can no longer be verified by `0.4.0`.
- **Console password hashes move to Argon2id in PHC format**: existing hashes keep working and are re-hashed the next time each user logs in. No action is needed.
- **`POST /v1/auth/login` requires `Content-Type: application/json`** and answers `415 unsupported_media_type` otherwise. The console, CLI and MCP already send JSON; update scripts that post a form.
- **Cross-origin browser requests are refused**: cookie-authenticated requests that a browser marks as cross-site now answer `403 csrf_failed` (in addition to the existing `X-CSRF-Token` check). Requests with `Authorization: Bearer` are not affected. If you embed the console behind another origin, serve it from the same origin.

### Added

- **PostgreSQL 19**: `postgres@19.0` runtime for Ubuntu 24.04 / 26.04 on amd64 / arm64. PostgreSQL 18 and 17 move to `postgres@18.7` and `postgres@17.12`.
- **PostgreSQL major version upgrades of a branch**: `branch upgrade` now supports PostgreSQL major version upgrades (including jumping majors such as `17 -> 19` and `18 -> 19`) via `pg_upgrade --swap` (PostgreSQL 18+, `--link` on older versions), preserving encoding, locale, and data checksum settings (`pg_controldata`), checking target extensions (including `anon`), logical replication slots/subscriptions, object-count/database-size warnings, and `pg_upgrade --check` compatibility, scaling operation timeouts from object counts, reporting `pg_upgrade` progress stages, rolling back automatically via ZFS checkpoints on failure, and running `vacuumdb --analyze-in-stages --missing-stats-only` in the background while keeping the branch `ready` (`stats_updating`, `stats_warning`, Raft Protocol `31`).
- `ext.AtomicStore` (`Get` / `PutIfAbsent` / `Take`) for modules that need atomic, leader-forwarded state (used by the SSO module).

### Security

- Console passwords use Argon2id in PHC format (`m=19456,t=2,p=1`), with transparent re-hashing of legacy hashes on login (Raft Protocol `32`).
- `net/http.CrossOriginProtection` is added on cookie-authenticated routes as defense in depth next to `X-CSRF-Token`.
- `POST /v1/auth/login` only accepts JSON (prevents login CSRF).
- Key, serial and certificate generation errors in the cluster PKI are no longer ignored.

### Fixed

- PGDG catalog generation compares versions with Debian rules (pre-releases such as `18~rc1` sort before `18.0`).
- Client connection files (DataGrip, MySQL Workbench, Navicat, DBeaver) are built with an XML encoder and use standard RFC 4122 UUIDv5 values, so control characters in names no longer break the files.
- The SQL runner handles MySQL conditional comments (`/*! ... */`) and `--` comments correctly.
- HTTP clients for artifacts and the license service have timeouts.
- `publish-apt.py` uploads `Release` / `Release.gpg` after the package indexes.
- The console's live updates reconnect with exponential backoff instead of retrying in a tight loop.

### Changed

- Dependencies replace hand-written code: `golang.org/x/term` (password prompts), `github.com/danieljoos/wincred` (Windows credential store), `golang.org/x/mod/semver` (catalog version comparison), `github.com/cenkalti/backoff/v5` (retries), `github.com/google/uuid`. All are pure Go (no cgo) under MIT or BSD licenses.
- Authentication state reads are cached and invalidated immediately when the store changes.

## [0.4.0] - 2026-11-26

1.0 (GA) readiness release introducing multi-user OSS console accounts, DB client connection file export in the Web Console, 100% Japanese Web Console localization, console TLS 1.3 hardening, API/CLI/MCP 1.0 contract unification, and token/cluster security improvements.

### Upgrade notes (read before upgrading)

- **Raft Protocol `26` → `30`**: upgrade nodes one at a time, followers first and the leader last (`apt update && apt upgrade`), then run `db-branch module install proxysql` (and any other installed module) once every node is on `0.4.0` so the cluster record matches. Until every node is upgraded, operations that need the new protocol answer `503 cluster_upgrading`: adding or changing console users, logging out (the local session ends; log out again after the upgrade), service token base/team restrictions, and the new join-token rules. Do not downgrade a node after the cluster has moved to protocol `30`.
- **Console requires TLS 1.3**: clients and reverse proxies that only speak TLS 1.2 can no longer connect to the console port (`:7080`). Existing self-signed certificates valid for more than 825 days are regenerated on start: trust the new certificate again (it is served at `/tls.pem`). To use your own certificate, set `[console] tls_cert` and `tls_key`.
- **API action names (1.0 contract)**: `branch.pause` and `branch.resume` are now separate actions, and checkpoint and snapshot deletes are `branch.checkpoint.delete` and `base.snapshot.delete`. The old names are still accepted as aliases for now; update custom authorizers, token policies and scripts to the new names. List endpoints always return `next_cursor` (`null` on the last page), and an unsupported engine feature answers `409 feature_not_supported`.
- **`branches`-scope API tokens are narrower**: they can call branch endpoints and `metrics.read` only. Cluster, license, module, update and extension read endpoints now answer `403`; use an admin token for those. Personal tokens carry their owner's role, so a token can no longer do more than its owner.
- **`base mask` is hidden** from the CLI, public API, Web Console and MCP in this release. Snapshots you already published keep working.
- **Join tokens** are single-use and expire after 24 hours by default (`cluster.join_token_ttl`); create a new one with `db-branch cluster token` for each node you add.
- **Legacy branches without an owner ID** (created before owner IDs were recorded and not migrated automatically) can be managed by an admin only.
- **MCP**: the branch argument is now `branch`; `name` still works but is deprecated.

### Added

- **Unlimited Multi-User OSS Console Accounts**: Core (without the paid `auth` module) now supports creating and managing unlimited console users (`db-branch user add/list/enable/disable/remove/reset-password`, `/v1/users` API, and the Web Console **Users** page).
  - All OSS users have admin-equivalent privileges (roles, teams, SSO, and base access restrictions remain in paid modules), with personal API tokens, `must_change_password` support, protection against removing the last active user, and seamless account migration when enabling/disabling the paid `auth` module (Raft Protocol `30`).
- **DB Client Connection File Export**: Added the **Client file** tab in the Web Console connection panel to generate connection files/URLs for DBeaver (`.dbp`), Chat2DB (`.json`), DataGrip (`dataSources.xml` + JDBC URL), Navicat (`.ncx`), pgAdmin 4 (`servers.json`), MySQL Workbench (`connections.xml`), and TablePlus URLs, supporting both direct and ProxySQL endpoints.
- **Console TLS Configuration & Hot Reload**: Added `[console] tls_cert` and `tls_key` options in `config.toml` with zero-restart certificate reload on TLS handshakes and `/tls.pem`, enforced TLS 1.3 minimum, and capped self-signed certificate validity to 825 days (auto-regenerating existing certificates exceeding 825 days on startup).
- **Lean 4 Formal Authorization Verification**: Integrated Lean 4 formal authorization model and theorems under `formal/lean/`, Go differential and route/alias coverage tests, and CI `lean` verification job.
- **Storage & Branch UI Enhancements**: Improved Storage tree alignment and displayed branch age and storage usage in the Web Console.

### Security

- **API Token Scope & Lifecycle**:
  - Replaced the `branches` token scope denylist with an explicit allowlist, rejecting admin/extension read endpoints (`ext.<module>.read`, `license.read`, `cluster.read`, `module.read`, `update.read`, etc.) with `403` while keeping `metrics.read` allowed.
  - Propagated non-admin owner roles onto personal tokens (`OwnerRoles`, Raft Protocol `28`) to prevent privilege escalation.
  - Revoked personal tokens immediately when users are deleted, and added periodic GC for expired tokens.
  - Enforced base and team restrictions on service tokens (Raft Protocol `29`).
- **Node API & Join Token Hardening**: Blocked `socket_only` routes (`POST /v1/auth/reset`, `POST /v1/backups/restore`, `POST /v1/cluster/token`) on `/internal/v1/api/` mTLS forwarding, and made cluster join tokens single-use with configurable TTL (`cluster.join_token_ttl`, default `24h`, Raft Protocol `27`).
- **Authorization & Mixed-Cluster Hardening**:
  - Removed display-name fallback ownership matching for unmigrated legacy branches (admin-only).
  - Returned `503 cluster_upgrading` with pending revocation flushing on `POST /v1/auth/logout` when cluster protocol is below `24`.
  - Added real-time principal resolution (`PrincipalResolver`), branch extend policy (`BranchExtendPolicy`), branch sharing (`BranchSharer`), `BranchTTLCap`, creation race protection, and `base.upload?base` authorization alignment.

### Changed

- **1.0 API / CLI / OpenAPI / MCP Contract Unification**:
  - Updated `api/openapi.yaml` to `1.0.0`, unified `next_cursor: null` across all list endpoints (`/v1/backups`, `/v1/auth/methods`, `/v1/public/bases`, `/v1/modules`, etc.), separated `branch.pause` / `branch.resume` and dotted checkpoint/snapshot delete action names (`branch.checkpoint.delete`, `base.snapshot.delete`) while preserving legacy action aliases for compatibility.
  - Mapped `ErrUnsupportedEngine` to `409 feature_not_supported`, unified MCP branch arguments on `branch` (keeping `name` as a deprecated alias), and fixed `branch create --runtime -o json` double JSON output and `base` name validation.
  - Fixed `base mask` cluster forwarding and port reservation bugs, and temporarily hid `base mask` from the 1.0 CLI, public API, Web Console, and MCP surfaces (preserving internal interfaces).
- **Documentation Sync**: Updated `SECURITY.md` (`db-branch-admin` group), `README.md`, `SUPPORT.md`, `CHANGELOG.md`, and `docs/` for 1.0 GA consistency (clarifying Ubuntu 24.04 / 26.04 LTS support), and added automated documentation verification tests in `scripts/docs_test.go`.

### Fixed

- **Web Console Localization & GA Fixes**:
  - Completed 100% Japanese localization across the Web Console (error-code-based translation with unknown-error fallback, tour, dialogs, `ja-JP` date/time formatting, and mobile 390px layout fixes including the `Client file` tab).
  - Subscribed to `setting.updated` / `module.updated` SSE events, handled `401` session expiration cleanly, fixed `Extend` buttons adding to remaining TTL instead of shortening it, restored `Branches` navigation and action-based button gating for Maintainer roles, aligned plan feature labels with `pkg/ext/license`, fixed `must_change_password` reloads on Portal/Admin, fixed RFC 3986 scheme URL password masking, unified `license` spelling, and displayed detailed Japanese `403` messages.
- **MySQL Admin SQL Binlog Isolation**: Executed MySQL administrative SQL (`CreateUser`, `fixup`, etc.) with `sql_log_bin = 0` so administrative statements do not pollute `history` module binlogs.
- **Debian Maintainer Scripts**: Branched `postinst`, `prerm`, `postrm`, and `db-branch-proxysql` scripts by `$1` lifecycle action so `upgrade` does not stop `db-branch.service` in `prerm`, missing `config.toml` does not fail upgrades, and `purge` removes package-generated configuration while preserving `/var/lib/db-branch` and ZFS datasets.

## [0.3.1] - 2026-11-24

Patch release following `v0.3.0` with faster `branch merge`, SQL query / Web SQL runner fixes, module sync hardening, and CI/packaging reliability improvements.

### Performance

- **Faster Branch Line Merge (`F-03`)**: `sqlmerge` now skips row-by-row comparison for tables whose digest is unchanged, speeding up `branch merge`.

### Fixed

- **SQL Query & Web SQL Runner (`W-11`)**:
  - Fixed a `sqlquery` panic on empty or comment-only inputs and improved `CRLF`, line comment, nested block comment, and `RETURNING` detection.
  - Added optional `database` targeting for PostgreSQL and MySQL across `POST /v1/branches/{name}/query`, MCP `query`, and the Web SQL runner, with database name validation to prevent DSN parameter injection.
  - Fixed Web SQL runner statement splitting for PostgreSQL JSONB `#` operators, dollar-quoted strings, trailing backslashes, and nested comments, and added **Stop** / unmount cancellation.
- **Module Sync & Rolling Upgrade (`F-04`)**:
  - Prevented downgrading installed packages that are already ahead of the cluster record during module sync and rolling updates.
  - Serialized `apt-get` / `apt-cache` operations via a context-aware semaphore to avoid lock contention.
  - Added bounded exponential backoff (1m to 30m) on module sync failures, along with SSE/notification events and `dbbranch_module_problem` metrics.
- **Restore & Merge Guidance (`F-05`)**: Clarified `branch restore` checkpoint behavior in documentation/UI and aligned `branch merge` error messages when no changes exist past the common ancestor.
- **CI & Packaging (`W-24`)**: Hardened `scripts/apt-restore-pool.sh` against transient HTTP errors and path traversal, pinned GitHub Actions to commit SHAs, enabled `go test -race ./...` in CI, and fixed test race conditions.

### Changed

- **Compatibility & Raft Protocol (`26`)**: No Raft protocol or state format changes since `0.3.0`.
- **Upgrade**: Upgrade cluster nodes one by one (leader last) via `apt-get update && apt-get install db-branch` (or `apt upgrade`). See [docs/distribution.md](docs/distribution.md) and [docs/updates.md](docs/updates.md).

## [0.3.0] - 2026-11-23

This release includes **Branch Fork**, **Branch Line Merge**, **External Runtime Modules (Experimental)**, and a comprehensive set of **security, reliability, and API/CLI hardening fixes** for GA readiness.

### Added

- **Branch Fork (`F-01`)**: Create an independent child branch from a running or paused branch's live state or from any existing checkpoint (`db-branch branch fork`, `POST /v1/branches/{name}/fork`, MCP `fork_branch`, and the Fork action in the web console). Supports `--checkpoint`, `--clean`, `--init-sql` / `--init-file`, `--ttl`, `--node`, `--owner`, `--access`, and `--record`. Uses Raft Protocol `25`. See `docs/branch-fork.md`.
- **Branch Line Merge (`F-02`)**: Logically 3-way merge changes from a past history line (or checkpoint) into the live line of the same branch (`db-branch branch merge`, `POST /v1/branches/{name}/merge`, MCP `merge_line`, and the Merge action in the web console), with `--dry-run`, `--checkpoint`, and conflict reporting at the table/row/key level without modifying the live line on failure. Uses Raft Protocol `26`. See `docs/branch-merge.md`.
- **Runtime Modules Standard (Experimental, single-node)**: Pluggable database engine contract (`RuntimeRegistry`, `RuntimeProvider`, `RuntimeCaps`) and gRPC protocol (`pkg/ext/proto/runtime/v1`) for external database engines, with Ed25519 signature verification, optional `allow_unsigned_runtimes` dev setting, dedicated unprivileged OS users (`db-branch-rt-<engine>`), systemd isolation (`db-branch-rt@.service`), conformance suite (`db-branch-conformance`), and CLI management (`db-branch module install|remove|list|trust`). Supports `branch upgrade` and source dump/load on external engines. Uses Raft Protocol `21`. Cluster usage is restricted/warned as single-node experimental in 1.0.
- `db-branch module forget <name>` (`POST /v1/modules/{name}/forget`, console **Forget** button): deletes the cluster record of a removed module so it is no longer listed (Raft Protocol `22`).
- **Cluster node removal (`W-17`)**: `db-branch cluster remove <node>` and `db-branch cluster leave` (`DELETE /v1/cluster/nodes/{name}`), refusing removal by default if bases or branches remain on the node unless `--force` is given.
- **License verification (`W-21`)**: Embedded production license root public key with offline `Verify` support and improved console TLS error handling.
- **Demo & documentation**: Refreshed browser demo (`demo.db-branch.com`) with path tracking, Team/Business interest contact links, GitHub Sponsors links, and updated `docs/roadmap.md`.

### Security

- **Cluster-wide & persistent logout (`R-01` / SEC-12)**: Console logout now revokes the session on every node and survives restarts via Raft (`revoke_session`, Raft Protocol `24`) and `revoked-sessions.json` on single nodes.
- **DB EnvironmentFile & systemd hardening (`R-04` / SEC-01, SEC-15)**: Database server environment files are now `root:db-branch` (`0640`) and DB systemd units are hardened (`NoNewPrivileges`, `ProtectSystem`, etc.).
- **Proxy & header hardening (`W-01` / SEC-02)**: Prevent `X-Db-Branch-Forwarded` spoofing and hop-by-hop header abuse across node forwarding.
- **Write-only secret settings (`W-02`)**: Secret settings (license key, open-branch password, webhook secrets) are write-only; `GET /v1/settings` and `setting.updated` return an empty `value` with `secret_set: true`. Use `clear` (`--clear <key>`) to remove a secret.
- **Operation & SSE ownership scoping (`W-03`)**: Non-admin users only see their own operations and SSE events.
- **Immutable branch owner IDs & visibility (`W-04`)**: `Branch.Owner` now stores the immutable principal ID (`OwnerName` holds the display name), with atomic cluster migration (`migrate_branch_owner`, Raft Protocol `23`). `branch.read` is scoped to own branches, `open` branches, and `SharesBranches` policies; `GET /v1/public/bases` 403 issue fixed.
- **Dump path & ExtPublic CSRF (`W-05`)**: Disallow local file paths for dumps over HTTPS and enforce CSRF and `must_change_password` checks on `ExtPublic` routes.
- **Audit logging & MCP guards (`W-06`)**: Filled missing audit log entries and added request guards and size limits on `POST /v1/mcp`.
- **Auth & DoS hardening (`W-07`)**: Fixed polling DoS on nonexistent tokens, global login lockout bypasses, and Argon2 lock contention.
- **Plugin isolation (`W-08`)**: Strengthened path, ownership, and environment variable isolation when loading extension modules.
- **Notification SSRF protection (`W-09`)**: Require HTTPS for webhook/Slack notifications, block redirects, always block loopback/link-local addresses, and block RFC1918/ULA by default (opt-in via `config.toml`).
- **Input limits & error hiding (`W-10`)**: Added input size limits, fixed TOCTOU on anonymous branch limits, and prevented internal error details from leaking in `500` responses.
- **SQL execution & credential safety (`W-12`)**: Prevented password leakage in `ExecAs`, handled `partial_revokes` suffixes, and fixed SQL escaping in `CreateUser`.
- **ProxySQL routing isolation (`W-19`)**: Fixed misrouted entries and username collisions; restricted `/*db_branch=...*/` comment routing to shared `RouteUser` connections only.

### Changed

- **Compatibility & Raft Protocol (`18` -> `26`)**:
  - Protocol `19`/`20` reservations were removed (`W-17`).
  - Protocol `21`: `ProtocolRuntimeModules` (external runtime module `Kind`/`Source` in `put_module`).
  - Protocol `22`: `ProtocolModuleForget` (`delete_module`).
  - Protocol `23`: `ProtocolBranchOwnerID` (`migrate_branch_owner`).
  - Protocol `24`: `ProtocolSessionRevocation` (`revoke_session`).
  - Protocol `25`: `ProtocolBranchFork` (`insert_branch`/`put_branch` with `ParentBranch`/`BranchForking`).
  - Protocol `26`: `ProtocolBranchMerge` (`insert_branch`/`put_branch` with `BranchMerging`).
  - In mixed-version clusters during a rolling upgrade, new commands wait until all nodes speak the required protocol; older behaviors (such as single-node-only logout) remain in effect until every node is at `0.3.0`.
- **Upgrade**: Upgrade cluster nodes one by one (leader last) via `apt-get update && apt-get install db-branch` (or `db-branch update`). No manual database or ZFS migration is required.
- **Extension compatibility**: `pkg/ext` (interfaces) and `pkg/ext/proto/runtime/v1` are the supported 1.0 extension contracts; `pkg/ext/plugin` (`net/rpc`) is internal/deprecated.
- **`base mask`**: Temporarily hidden from CLI/API/Web Console/MCP for 1.0 while keeping `pkg/ext` interfaces intact.

### Fixed

- **DB lifecycle & recovery (`W-13`, `W-14`)**: Fixed DB server start/stop/recovery edge cases and state consistency across agent restarts.
- **Resource exhaustion (`W-15`)**: Excluded paused branches from Placement memory budgets (checked on resume), enforced optional `refquota`, added pool free-space guard (10% or 1 GiB minimum), serialized concurrent uploads, and cleaned up `srcdump` temporaries.
- **Cluster stability (`W-16`)**: Fixed backup directory, Raft store, and base replication bugs.
- **Cluster updates (`W-18`)**: Added module version checks, post-cancellation cleanup, and `dpkg` lock contention prevention during rolling updates.
- **Runtime module upgrades (`W-20`)**: Preserved trusted keys when old directory removal fails during runtime module upgrade, and added single-node warnings in clusters.
- The console's Modules page no longer marks free modules (for example ProxySQL) as **Paid** when the licence covers every module.
- Text settings in the console keep trailing spaces and newlines (`W-02`).

### Removed

- Removed the `db-branch-anon`, `db-branch-greenmask`, and `db-branch-greenmask-mysql` masker modules from this repository; they are now maintained in separate repositories. The `Masker` extension point (`pkg/ext`) remains in the core repository.

## [0.2.7] - 2026-11-18

This is the security fix first tagged v0.2.6. That release run stopped at a test-coverage check before anything was published, and the tag cannot be moved, so the release is 0.2.7. There is no 0.2.6 package.

### Security

- **Anyone who could reach the console or API port could run commands as root on a node.** A branch made from a public snapshot needs no login, and the SQL it takes (`init_sql`) ran through `psql` or `mysql` as the agent (root): a client command in it (`\!` in `psql`, `system` in `mysql`) ran on the node. On PostgreSQL, the branch login was a superuser as well, so `COPY ... TO PROGRAM` ran commands as the `db-branch` OS user, which could use the agent's socket and reset the admin password. Fixed:
  - `init_sql` runs as the branch login, never as the admin. On PostgreSQL the server reads the SQL (no `psql`: only plain SQL and `\connect` are accepted); on MySQL the client runs with `--binary-mode`, which switches its commands off.
  - A call without a login may not send `init_sql` (`403`).
  - The PostgreSQL branch login is no longer a superuser: it is a member of a `db_branch_owner` role that owns the objects in the databases, so it can still create, alter and drop them. The MySQL branch login has no `FILE`, `SUPER`, `SHUTDOWN`, `RELOAD`, `PROCESS`, `CREATE USER`, replication or `GRANT OPTION` privilege, and `my.cnf` sets `secure_file_priv=NULL` and `local_infile=OFF`.
  - Branches that exist already are put right when the agent starts and whenever a branch server starts (so after a reset, a restore or a resume too).
  - The agent's socket and `/etc/db-branch/config.toml` belong to a new group, `db-branch-admin`, not to `db-branch`, the group the database servers run in. The package adds people who were in `db-branch` to `db-branch-admin`; without that group the socket is root's alone. The database server units cannot see `/run/db-branch` and `/etc/db-branch`.
  - What changes for you: `init_sql` can no longer use `CREATE USER`, `GRANT`, `COPY ... PROGRAM`, `CREATE EXTENSION` of an extension that is not trusted, or psql commands other than `\connect`; a branch login is no longer allowed to do what a superuser can. Run such work as the admin on a base, before the snapshot.

## [0.2.5] - 2026-11-15

### Fixed

- `db-branch runtime install mysql@8.0.46` (and `mysql@8.4.13`, `mysql@9.6.0`) works on arm64 (Apple Silicon VMs, Graviton): the catalog had no arm64 entry, so the install failed with `runtime not found` and the Multipass evaluation VM stopped before writing /etc/motd. Oracle ships no `-minimal` build for aarch64, so arm64 uses the full `glibc2.28-aarch64` tarball (800 MB to 1 GB to download).
- Installing a MySQL runtime drops the debug server and plugins of the full upstream tarballs (`bin/*-debug`, `lib/plugin/debug`), which db-branch never runs: about 500 MB less per version on arm64 (8.4.13: 1.5 GB to 1.1 GB).
- Evaluation VM: when a setup step fails, `/etc/motd` says so and points at `/var/log/cloud-init-output.log` (it used to stay empty). The README adds `--timeout 900` to `multipass launch` (arm64 downloads about 800 MB) and a one-line form for Windows.

## [0.2.4] - 2026-11-15

### Added

- `db-branch init --evaluation`: the first admin password is `admin` and the console asks for a new one (12 characters or more) at the first sign-in; until then only the session, logout and password calls work (403 `password_change_required` otherwise; `must_change_password` in the session). For a throwaway evaluation VM.
- Web console: an **SQL** section on the page of a branch (admin console and user page) runs statements separated by `;` one after another on the branch as its own user (it stops at the first that fails) and shows, for each, the rows as a table (up to 500) or the rows affected, with the time. It uses `POST /v1/branches/{name}/query` and shows for a `ready` branch the person may query.
- **Pause / Resume of a branch**: `branch pause <name>` (`POST /v1/branches/{name}/pause`) stops the branch's database server cleanly and keeps its data, so no memory is held; `branch resume <name>` starts it again on the same port with the same credentials. A paused branch has the status `paused` (`pausing` and `resuming` while it changes), refuses connections, and cannot be reset, checkpointed or upgraded until resumed; it can be deleted, and its expiry keeps running. The console has Pause / Resume next to Extend. Cluster protocol 18: a pause is refused with `cluster_upgrading` until every node runs this version.
- Web console (admin): a **Storage** page (`/admin/storage`) shows how much of a node's pool is used as a tree: each base with its data and snapshots, and under each snapshot the branches cloned from it, every line with its size and its share of the pool (branches whose snapshot is gone sit under their base, branches without a base in a "No base" group). The API gets `GET /v1/nodes/{name}/storage`, a flat list of the datasets and snapshots under db-branch's root with their space use.

## [0.2.3] - 2026-11-14

### Added

- Web console (admin): the Branches and Bases lists have a search box and filters (Branches: status, engine, base, owner; Bases: status, engine, node), kept in the address (`?q=…&status=…`).
- Web console (admin): every base has a page of its own at `/admin/bases/<name>`; the detail drawer of the Bases list became that page, and the cards of the list open it. The node page of the cluster shows the OS and ZFS once (the distribution key is the tooltip; the ZFS kernel module is named only when it is another release), and the account button in the sidebar no longer overflows.
- MySQL 5.7 and 5.6 for migration testing: `db-branch runtime install mysql@5.7.44` (and `mysql@5.6.51`) installs the official generic tarballs (amd64 only; Oracle ships no arm64 build of 5.x). Branches of them can be created, saved as checkpoints and upgraded 5.6 to 5.7 to 8.0 (a series is never skipped), with pre-checks for old password hashes and for partitions outside InnoDB, and `mysql_upgrade` run after an upgrade to 5.7. The history module does not record 5.x branches (it says so when asked to); masking has not been checked on 5.x.
- Web console: every branch has a page of its own at `/admin/branches/<name>`, with its facts, its actions (recreate, copy proxy URL, extend, reset, delete), the connection details, the Checkpoints graph, the recorded transactions as a searchable table (history module) and the upgrade. The Branches list is now a light table (status, name, base) whose names open the page; its action menu and the Connect and Checkpoints dialogs moved to the page.
- Web console, user side (the portal): a branch row in "Your branches" is a summary that opens the branch's own page at `/branch/<name>` instead of expanding in place; the details it showed, the Checkpoints graph, and (history module, recorded branches) the transactions table are on that page. A new branch opens its page once it is ready.

### Fixed

- Web console: the transaction dots (and groups) of a line that continues another one's lane started at the left edge of the graph, apart from the lane, instead of where the lane has got to.
- Web console: the Upgrade box of a branch floats under its button instead of pushing the other facts around when it opens.
- Web console: the Upgrade panel and the transactions views use short labels instead of explanatory sentences ("Upgraded to mysql@8.0.46 · before-upgrade-…", "Check passed", "Rolled back", "Not installed"); the confirmation shows only the two runtimes.
- Web console: the Upgrade panel of a branch now follows the server's upgrade rules (5.6 to 5.7 to 8.0; it still said upgrades start from 8.0, so a 5.7 branch had no target), and it lists the runtimes the branch could move to that are not installed on the node yet, each with its `db-branch runtime install` command.
- Web console: a lane that forked off another now starts right next to the node it forked from and its checkpoints follow one slot apart, instead of being placed where they fall in time against all the other lanes, which pushed new lines far to the right.
- Web console: the branch drawn for a restore from the middle of a line now leaves its lane at a steep, nearly right angle and then runs straight (dashed) along the new lane, instead of a long curve.
- Web console: recorded transactions made less than a second apart (for example the rows of one save) are drawn as one group in the Checkpoints graph, with a count, instead of several dots on top of each other. Selecting the group lists its transactions in time order as a table, and each row can be chosen to restore to (a single Restore to #N button below the table). A DDL statement or a checkpoint ends a group. When a stretch between two nodes is too short for all its dots, the closest neighbours are merged into groups (a DDL statement never is) instead of overlapping or being left out; the Busy-stretches sampling is gone.
- Web console: a branch that records its transactions now shows the Checkpoints graph as soon as it has a recorded transaction, also right after a reset when it has no checkpoint yet. The graph used to appear only with a checkpoint or a second line.

### Changed

- No plan includes the diff module any more (it was in Lite, Team and Business). Like masking governance it is an opt-in: only a licence whose `Modules` names `diff` enables it (a plain key or a trial does not). Licences that used diff through their plan stop being accepted by it.


## [0.2.2] - 2026-11-14

### Added

- Web console: an Expand button on the Checkpoints graph opens the whole Checkpoints panel (graph, selected node and its actions, save, list, confirmations, messages) in a modal. It shares its state with the panel on the page, locks page scroll, traps focus, closes with Esc, Close or a click outside, and is full screen on a phone.

### Changed

- Console: restoring the last recorded transaction of a line (history module) continues that line instead of drawing a new one, like the restore of a tip checkpoint. The history module now remembers which transaction a restore went to and returns it as `from_tx` in the events response of that line (only while the line's log holds it); the console takes it as the restored point and extends the lane when nothing on the lane is later. Without `from_tx` the line forks as before. Display only; the core API did not change.
- **A restore no longer saves the current state automatically.** Restoring a checkpoint (`POST /v1/branches/{name}/checkpoints/{checkpoint}/restore`, `branch restore`, MCP `restore_checkpoint`, and a module's restore through the core) replaces the branch's current state and takes no `before-restore-<time>` checkpoint any more; the work after the last checkpoint is gone unless the person saved a checkpoint first. Every checkpoint taken so far stays and can be restored as often as before, `before-upgrade-<time>` is unchanged, and `before-restore-*` checkpoints made by older versions stay (listed, restorable, deletable) and are still not counted toward `branch.max_checkpoints`. The product only nudges: the console's restore confirmation (checkpoint and recorded transaction, also inside the Expand modal) says the state is replaced and not saved and has a **Save a checkpoint first** button; `db-branch branch restore` prints one note on standard error before it starts (no question, exit status unchanged); the MCP instructions and the `restore_checkpoint` description tell the agent to call `create_checkpoint` first. The operation no longer has the phase `checkpointing`. No API field and no Raft command changed. A failed restore before the swap still leaves the branch as it was, running.
- Web console: restoring the tip of a line of history no longer adds a new lane to the graph. The restored state continues on the same lane, with no fork and no new "line N" label. Restoring a point in the middle of a line still forks a new lane. Display only: the API and the server are unchanged.

## [0.2.1] - 2026-11-13

### Changed

- No functional change from 0.2.0. This is the first release built and published from the recreated repository, which checks the release pipeline (signing, publishing to the APT repository, and the check that the published version can be downloaded).

## [0.2.0] - 2026-11-13 - first public release (Beta)

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
- `api/openapi.yaml` is now a complete OpenAPI 3.1 description of the core REST API, good enough to render with Redoc as the public API reference (version 0.2.0, was a 28-path draft that no longer matched the agent). It lists all 87 routes the agent serves with request and response schemas derived from the handlers, examples, the error codes in use, the authentication of each call (session cookie, API token, local socket, anonymous), the `x-dbb-action` and the token scopes that may call it, and tag groups (`x-tagGroups`). It covers checkpoints, branch upgrade, recording, base masking and refresh, anonymous limits, settings, tokens, modules, licence, backups, updates, metrics, MCP and the account endpoints; module APIs under `/v1/ext/` are documented by each module. A test (`internal/interface/httpapi/openapi_test.go`) fails when a route has no operation in the file or the file lists a route the server does not serve. `/v1/mcp` is now registered for POST only (the stateless MCP handler answered other methods with 405 anyway).
- The licence renewal request carries an anonymous cluster id: `POST <renew_url>` now sends `{"key": "...", "cluster": "<id>"}`, where `<id>` is 32 lowercase hex characters drawn at random (crypto/rand) the first time the leader renews, and identical on every node and across restarts, leader changes, node joins and upgrades. It is not derived from any host, address, secret or key, and it is not shown in the API or the console. The licence service uses it only to notice a key used by more than one cluster and to tell its owner; it never blocks a renewal (a service that ignores the field works as before; the field is left out while no id exists yet). No setting is added: `license.auto_renew` off still means no connection at all. New Raft command `init_cluster_id` (set-if-empty, so two leaders racing end with one id), **protocol 17**: an existing cluster gets its id lazily once every node runs 17, and until then renewals are sent without it; older nodes skip the new `cluster_id` state field. A single node (`state.json`) keeps its id when it becomes a cluster. `licensing.RenewClient.Renew` takes the id as a fourth argument. See `docs/license-renewal.md`.
- Per-branch database upgrades (core, Apache-2.0). `db-branch branch upgrade <name> --runtime <engine@version>` moves one branch to a newer runtime of its engine, mainly MySQL 8.0 to 8.4, to test a migration and go back if needed; the base and the other branches are untouched. Optional pre-check (also on its own with `--check-only`, which changes nothing) listing findings as `{level, code, message}`: accounts using `mysql_native_password` (8.4 keeps the plugin loaded for the branch so they still work; 9.x converts the branch's own account and flags the others as an error), the generated my.cnf checked by the target `mysqld --validate-config` with removed options (`default_authentication_plugin`, `expire_logs_days`, `master_info_repository`, ...) dropped or rewritten, and a free-space estimate. Errors block the upgrade unless `--force`. Then the server is stopped cleanly, the automatic checkpoint `before-upgrade-<time>` is saved (kept like `before-restore-*`, not counted toward `branch.max_checkpoints`), the server starts on the new binaries (MySQL upgrades its data dictionary itself), and the warnings and errors it logged are returned in the operation's `result`. A failure (will not start, upgrade error, timeout) restores the checkpoint and starts the old runtime again with the error log excerpt kept in the result; `--no-rollback` keeps the failed state. Allowed: the same engine, upgrades only, MySQL 8.0 to 8.4, 8.4 to 9.x and patch upgrades (skipping a long-term-support series such as 8.0 to 9.x is refused), PostgreSQL minor upgrades within one major (major upgrades: not supported yet). The target must be installed on the branch's node (the error says how to install it). `branch create --runtime` creates a branch and upgrades it at once. New API `POST /v1/branches/{name}/upgrade` (`runtime`, `check_only`, `no_rollback`, `force`; operation kind `branch.upgrade`, action `branch.upgrade` for the owner and admins), a `runtime` field on branch creation, an operation `result` field, `base_runtime` on the branch JSON, `runtime` on checkpoints, branch status `upgrading`, MCP tools `check_upgrade` and `upgrade_branch`, and an Upgrade button with a confirm dialog and a result view in the console. A branch's runtime is now its own: it survives restarts and failover, checkpoints record the runtime they were taken with and restoring one switches back to it, and reset (and recreating a lost branch) returns to the base's runtime. Cluster protocol 16: the feature waits until every node runs it. ProxySQL keeps advertising its configured server version. See `docs/upgrade.md`.
- Each node's OS, kernel and ZFS version in the cluster view: `GET /v1/cluster` returns a `system` object per node (`distro`, `os`, `kernel`, `zfs`; `unknown` for nodes of older versions), `db-branch cluster status` has an OS column, and the console's Cluster page shows OS, kernel and ZFS per node. The facts travel in the existing node heartbeat (no new Raft command, no protocol change) and are cached.
- Mixed-OS safety for PostgreSQL. A PostgreSQL data directory depends on the system's collation libraries (glibc locale data, ICU), so a base snapshot now records the distribution of the node it was made on, and branches (including recreates after a node loss) are placed only on nodes of that distribution. Choosing a node of another distribution with `--node`, or listing one in a PostgreSQL base's `replica_nodes`, is refused with `409`; such nodes no longer receive copies of PostgreSQL bases. MySQL is not affected (it ships its own collations). Nodes that do not report a distribution are never excluded. See `docs/mixed-os.md`.
- Branch checkpoints. Save a running branch's state (a ZFS snapshot; instant, the server keeps running) and put the branch back to it later, from the console (a Checkpoints section in each branch card and in the admin Branches page), the CLI (`branch checkpoint`, `checkpoints`, `restore`, `checkpoint delete`), the API (`/v1/branches/{name}/checkpoints`) and MCP (`create_checkpoint`, `list_checkpoints`, `restore_checkpoint`). Restoring loses nothing: the current state is saved as a `before-restore-<time>` checkpoint, every other checkpoint stays restorable any number of times, and an interrupted restore is finished when the agent restarts. Reset keeps the checkpoints, deleting a branch removes them, and the new `branch.max_checkpoints` setting (default 20, counting only the checkpoints you take) limits them. The automatic `before-restore` checkpoints are not counted and never block a restore: the newest five are kept and a restore that adds a sixth deletes the oldest. Saving a checkpoint warns (in the API answer, the console and the CLI) when the branch's data and history use more than half of the pool's free space. New actions `branch.checkpoint.read`, `branch.checkpoint`, `branch.restore` and `branch.checkpoint_delete` are for the owner and admins only. Needs the node's own ZFS (not module storage): the branch JSON says so with `checkpoints: true|false` (a cluster node reports it with its heartbeat), the console shows checkpoint controls only where it is true, and the endpoints answer `409 checkpoints_unavailable` elsewhere. See `docs/checkpoints.md`.
- Extension points for the paid `history` module (additive; existing modules are unaffected): `ext.ServerOptioner` adds lines to a branch server's configuration, `ext.RestoreFinisher` lets a module work on restored data while the server is in maintenance mode (no network), and `ext.Host.Branches` (`ext.BranchHost`) gives a module this node's branches: checkpoints and lines, restore (also of the creation state) with a note, the server's admin socket, read-only mounts of any line or checkpoint, and operations of its own. `/v1/ext/<module>/branches/<name>/...` is answered by the node holding the branch. New `license.FeatureHistory`. See `docs/modules.md`.
- Masking governance (the `masking` module) is no longer part of the Business plan or the trial: no plan includes it at launch, and it will return as an opt-in beta. `License.Has(FeatureMasking)` is true only for a licence whose `modules` field names `masking` explicitly (`*` does not count); the constant and the module code stay. The console's Business label no longer lists masking. The free maskers (`greenmask`, `greenmask-mysql`, `anon`) are unaffected.
- New licence plan `lite` (history and diff only, up to 3 nodes) below Team; Team and Business include history and diff (`planFeatures` and `planNodes` in `pkg/ext/license`). The console's licence card shows it as "Lite · history, diff".
- Extension points for the paid `diff` module (additive; existing modules are unaffected): `ext.Host.API` (`ext.ModuleAPI`) lets a module call its own API through the agent, as a principal it names and the way a client would (authorized as `ext.<module>.read`/`.write`, writes audited, and forwarded to the node holding the branch for `/branches/<name>/...` or `?node=`), which is how a module on one node asks the same module on another node for work on a branch there. `ext.BranchState` also carries `base` and `snapshot`. New `license.FeatureDiff` (`diff`, in the Team and Business plans). See `docs/modules.md`.
- `POST /v1/branches` takes `record` (CLI `branch create --record`, MCP `create_branch`), `GET /v1/capabilities` says whether recording is available (`record`), and the branch carries `record`. `GET /v1/branches/{name}/checkpoints` also returns the structure of the lines (`lines`, `live`, `head`), which the console draws as a history graph. A branch record gains the `record` field: in a cluster with older nodes still running, an older node that rewrites the record drops it, so upgrade every node before relying on it.
- Public-release documentation: README, SECURITY, SUPPORT, CODE_OF_CONDUCT, issue and pull request templates, Dependabot configuration, and an English index of `docs/`.

### Changed

- The diff module is labelled beta: the console's licence card reads "Lite · history, diff (beta)" (and Team likewise). It stays in the Lite, Team and Business plans.
- Whether a branch records its changes (history module) is now decided once, when the branch is created, and can never change, so the change log has no gaps. A branch records if it is created with `record: true` or if the module setting `history.record_all` is on at that moment; later changes of `record_all` affect only branches created after them (before, it also applied to existing branches at their next start). No API, CLI or console path turns recording on or off for an existing branch (stores keep the stored value when a branch record is rewritten). A recorded branch does not start while the history module is missing, disabled or unlicensed, on create, recreate, reset, checkpoint restore and every restart: it is `failed` with the reason in `status_reason`, and becomes `ready` at the next start once the module is back. The branch views show it read-only (`record` in the API, `Recording: on (since creation)` in `branch show` and the console, where the Record checkbox is only in the create form). Extension point: `ext.ServerBranch.Creating` and `ext.ServerOptions.Record` let a module answer, once at creation, that a new branch is to be recorded (additive). No Raft protocol change.

### Fixed

- Branches of a freshly loaded base no longer run with the statistics of empty tables. A base snapshot was taken right after the dump was loaded, before InnoDB had computed the statistics of the large tables in the background (`mysql.innodb_table_stats` still said `n_rows = 0`), and every branch inherited them: MySQL then planned range queries as full scans (on the 2.8-million-row `salaries` table of the employees sample, a range of 137 rows read the whole primary key), which made the diff module and any application query slow. Before every base snapshot (create, `base snapshot`, scheduled refresh, masked bases) the agent now updates the statistics while the server still runs: MySQL `mysqlcheck --analyze --all-databases`, PostgreSQL `vacuumdb --all --freeze --analyze` (which also marks the rows visible and frozen, so that branches do not rewrite every page they read to set hint bits). New phase `analyzing`; a stopped base server is started for it and stopped again. Existing bases get it with their next snapshot (`db-branch base snapshot <name>`); branches made earlier keep their statistics until `ANALYZE TABLE` / `ANALYZE` is run in them.
- A new branch is reachable through the proxy (ProxySQL, ports 6033/6133) as soon as it is ready. Each node re-applied the proxy configuration only every 5 seconds, so a connection through the proxy failed for up to about 5 seconds after `branch create` returned (about 3 to 4 seconds on average in a four-node test), which made creating and connecting take up to about 10 seconds. Every node now applies it on each branch change it receives (the 5-second pass stays as a fallback). Direct connections to the branch port were not affected.
- Branch servers no longer fail now and then with `Can't start server: Bind on TCP/IP port: Address already in use`. The branch ports (`[ports]`, default 40000-40999) lie inside Linux's ephemeral port range (32768-60999), so an outgoing connection of any process could hold the port a new branch was given. The agent now adds the branch port range to `net.ipv4.ip_local_reserved_ports` when it starts and when the `ports.min`/`ports.max` settings change, keeping what is already reserved (the kernel then no longer picks those ports for outgoing connections; servers bind them as before). The setting is not persisted across reboots by the package; the agent sets it again at every start.
- A PostgreSQL base's holder status no longer shows nodes of another distribution as `pulling`. Such nodes were already skipped by placement and replication (nothing was ever copied), but `db-branch base show`, the API base view (`holders`) and the console still counted them as wanted holders, so they looked stuck (this includes bases with replicas on "all nodes" and legacy snapshots without a recorded distro, where the base node's distro applies). Those nodes now have state `skipped`, with `reason` (`other OS: ubuntu-26.04`) and `distro` (additive fields); the CLI prints `skipped (other OS: ubuntu-26.04)` and the console greys the node with the reason. `dbbranch_base_replicas_missing` no longer counts them, nor nodes the base's `replica_nodes` leaves out. MySQL bases are unaffected. See `docs/mixed-os.md`.
- Branches no longer hang in `provisioning` on a node whose `db-branch` user has other numeric ids than the node the base came from. The package creates the `db-branch` user with whatever uid/gid is free, and bases are copied between nodes with `zfs send/receive`, which keeps numeric owners; on such a node mysqld failed with `File './binlog.index' not found (OS errno 13 - Permission denied)`. The cluster now agrees on one uid/gid for `db-branch`: it is recorded in the cluster state (new Raft command `init_service_ids`, **protocol 15**; the first leader records its own ids, an existing cluster adopts the leader's once every node is upgraded). `db-branch cluster join` compares the joining node's ids with the cluster's before any data is received and, if they differ and the cluster's ids are free locally, stops the node's db-branch services, re-owns `/var/lib/db-branch`, `/etc/db-branch`, `/run/db-branch*` and the node's ZFS dataset mounts, and runs `groupmod`/`usermod`; if another account has those ids, the join is refused with instructions and nothing is changed. Existing nodes that disagree are reported (heartbeat `service_ids` / `id_problem`, `db-branch cluster status`, the console's Cluster page), take no new branches or replicas (`409` with the reason when named with `--node`), and are fixed with the new `db-branch cluster align-ids`. A server whose data directory belongs to another uid now fails the branch at once with a clear `status_reason` (`data directory ownership mismatch ... see db-branch cluster status`), as does any server that exits while starting, instead of waiting out the start timeout. The package does not hard-code a uid (it could collide with an existing account). See `docs/service-user-ids.md`.
