# db-branch

Instant database branches for MySQL and PostgreSQL, built on ZFS clones.

**Status: Beta.** db-branch is usable today, but APIs and command-line details may still change before 1.0.

db-branch is an independent open-source project, built from scratch. Create a copy-on-write branch of a production-sized database, break it, reset it, and throw it away, without copying any data.

## What it does

- **Branches in seconds.** On our own 3-node test cluster a PostgreSQL branch is ready in about 0.5 s and a MySQL branch in about 3 s, with 0 bytes copied at creation (copy-on-write).
- **Reset and expire.** `branch reset` returns a branch to its creation state; `--ttl` deletes it automatically.
- **MySQL and PostgreSQL, several versions at once.** Engine runtimes are installed by version and can live side by side.
- **One interface everywhere.** CLI, REST API, web console and an MCP server (so LLM agents can create and query their own branches) all use the same API.
- **Single node or cluster.** A Raft cluster of VMs, with optional ProxySQL routing so applications keep one address.

## Try it

A live console demo runs entirely in your browser (nothing real is created): <https://demo.db-branch.com/>

## Install

Requirements: Ubuntu 24.04 LTS or 26.04 LTS, and a ZFS pool (the `zfsutils-linux` package is installed as a dependency).

```sh
curl -fsSL https://apt.db-branch.com/db-branch.gpg | sudo tee /usr/share/keyrings/db-branch.gpg >/dev/null
echo "deb [signed-by=/usr/share/keyrings/db-branch.gpg] https://apt.db-branch.com stable main" | sudo tee /etc/apt/sources.list.d/db-branch.list
sudo apt update && sudo apt install db-branch
sudo db-branch init --pool <zpool>
```

The repository signing key fingerprint is `3F404C4E5D49FA653F8385692C6E89E341D4D7A5`. `db-branch init` prints the initial admin password once.

## First branch

```sh
db-branch runtime install postgres@18.6                                     # a database engine version
db-branch base create prod --runtime postgres@18.6 --dump prod.sql.gz       # a base: your data, loaded once
db-branch branch create feature-x --base prod --ttl 72h                     # a branch: instant, copy-on-write
db-branch branch dsn feature-x                                              # connection URL (includes the password)
db-branch branch reset feature-x                                            # discard every change
db-branch branch delete feature-x                                           # remove it
```

MySQL works the same way, for example `mysql@8.4.13`. List installable versions with `db-branch runtime available`.

## How it works

A base is a ZFS dataset holding a loaded database. A branch is a ZFS clone of a base snapshot, so creating one copies nothing, and each branch runs its own database server on its own port. Optionally, ProxySQL routes client connections to the right branch by user name (or by a `/*db_branch='name'*/` query comment). Several nodes form a Raft cluster that shares bases, branches, settings and users.

## Editions

- **Core (this repository), Apache-2.0:** branches, bases, runtimes, clustering, CLI, API, web console, MCP, monitoring, notifications, anonymous (no sign-in) branches with limits, scheduled base refresh, backup of cluster state.
- **Free modules** in the same apt repository: `proxysql`, `greenmask`, `greenmask-mysql`, `anon` (routing and data masking).
- **Paid modules** (auth/SSO, audit, masking governance, FSx storage) are separate, closed-source packages, self-hosted and enabled with a licence key (monthly, yearly or one-time). See [docs/modules.md](docs/modules.md) for the extension points they use.

There is no hosted or cloud version of db-branch.

## Documentation

- [docs/README.md](docs/README.md): index of the design and reference documents (written in Japanese)
- English user documentation: <https://db-branch.com/docs/>
- Website: <https://db-branch.com> (English), <https://db-branch.com/ja/> (Japanese)
- [api/openapi.yaml](api/openapi.yaml): machine-readable REST API definition
- [CHANGELOG.md](CHANGELOG.md)

## Contributing, security, support

- Contributions are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md). Commits need a DCO sign-off (`git commit -s`).
- Security issues: see [SECURITY.md](SECURITY.md). Please use private vulnerability reporting, not public issues.
- Support: see [SUPPORT.md](SUPPORT.md). No support is promised; Issues are read when possible.
- Please follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

## 日本語

db-branch は、ZFS クローンで MySQL / PostgreSQL のデータベースブランチを発行・リセット・破棄するツールです。現在は Beta で、1.0 までに API が変わる可能性があります。コアは Apache-2.0 で、有償モジュール（認証/SSO・監査・マスキングのガバナンス・FSx ストレージ）は別パッケージ（クローズドソース、セルフホスト、ライセンスキー）です。日本語のサイトは <https://db-branch.com/ja/>、設計ドキュメントは [docs/](docs/) にあります（日本語）。
