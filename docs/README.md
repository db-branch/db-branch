# Documentation index

The documents in this directory are written in **Japanese**. English user documentation is at <https://db-branch.com/docs/getting-started/>. Japanese documentation is at <https://db-branch.com/ja/>.

| File | What it covers |
|---|---|
| [design.md](design.md) | Overall design: goals, terminology, architecture, engines and runtimes, branch lifecycle, base snapshots, ZFS tuning, ProxySQL routing, clustering, packaging, the web console, extension modules |
| [api.md](api.md) | REST API design: conventions, resources, operations and events, with examples (machine-readable definition: [../api/openapi.yaml](../api/openapi.yaml)) |
| [checkpoints.md](checkpoints.md) | Branch checkpoints: saving a branch's state and restoring it without losing the others (dataset layout, restore steps, crash safety, limits) |
| [upgrade.md](upgrade.md) | Per-branch database engine upgrades: pre-checks, automatic checkpoints and rollback, supported paths |
| [branch-fork.md](branch-fork.md) | Branch forking: cloning an existing running or paused branch (or one of its checkpoints) into a new independent child branch |
| [branch-merge.md](branch-merge.md) | Branch line merging: logical 3-way merge of row and schema changes from a past history line (or checkpoint) into the live line within a single branch |
| [resource-limits.md](resource-limits.md) | Resource limits: ZFS `refquota`, per-user branch caps, max TTL, pool free-space guard, memory budgets, refresh dump caps |
| [cluster-plan.md](cluster-plan.md) | Implementation plan for clustering: ports, steps and the verification done at each step |
| [mixed-os.md](mixed-os.md) | Running nodes of different distributions in one cluster: what each node reports and why PostgreSQL branches stay on the distribution their base was made on |
| [service-user-ids.md](service-user-ids.md) | Why every node of a cluster must use the same db-branch uid/gid, how `cluster join` and `cluster align-ids` align them, and how mismatches are reported |
| [base-refresh.md](base-refresh.md) | Reloading a base from a source database on a schedule |
| [masking.md](masking.md) | Data masking workflow and external masker modules (separate repositories; `base mask` is hidden in 1.0) |
| [modules.md](modules.md) | Writing and deploying extension modules: ports, plugin protocol, settings, module APIs and pages, cluster-wide install |
| [runtime-module-install.md](runtime-module-install.md) | Installing, verifying, trusting and removing external runtime modules from GitHub Releases (Experimental, single-node in 1.0) |
| [runtime-modules-standard.md](runtime-modules-standard.md) | Standard specification for external third-party database engine runtime modules (`RuntimeProvider` / gRPC contract) |
| [mcp.md](mcp.md) | The MCP server for LLM agents: endpoint, tokens and scopes, tools, connecting Claude Code |
| [monitoring.md](monitoring.md) | Prometheus metrics and health endpoints |
| [notifications.md](notifications.md) | Slack and webhook notifications and the events that trigger them |
| [updates.md](updates.md) | Minor-version cluster updates from the console or CLI |
| [client-cli.md](client-cli.md) | Standalone remote client CLI (`login`, profiles, TOFU TLS, `connect`, cross-platform release & Homebrew/Scoop) |
| [distribution.md](distribution.md) | The apt repository: user steps, contents, release pipeline and signing key handling |
| [license-renewal.md](license-renewal.md) | Monthly and yearly license keys and how automatic renewal works |
| [roadmap.md](roadmap.md) | Public roadmap toward 1.0 GA and post-1.0 plans |
| [seo-indexing-report.md](seo-indexing-report.md) | Historical investigation report on `db-branch.com` search indexing |
