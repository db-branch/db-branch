# Documentation index

The documents in this directory are written in **Japanese**. English user documentation is at <https://db-branch.com/docs/getting-started/>. Japanese documentation is at <https://db-branch.com/ja/>.

| File | What it covers |
|---|---|
| [design.md](design.md) | Overall design: goals, terminology, architecture, engines and runtimes, branch lifecycle, base snapshots, ZFS tuning, ProxySQL routing, clustering, packaging, the web console, extension modules |
| [api.md](api.md) | REST API design: conventions, resources, operations and events, with examples (machine-readable definition: [../api/openapi.yaml](../api/openapi.yaml)) |
| [checkpoints.md](checkpoints.md) | Branch checkpoints: saving a branch's state and restoring it without losing the others (dataset layout, restore steps, crash safety, limits) |
| [cluster-plan.md](cluster-plan.md) | Implementation plan for clustering: ports, steps and the verification done at each step |
| [mixed-os.md](mixed-os.md) | Running nodes of different distributions in one cluster: what each node reports and why PostgreSQL branches stay on the distribution their base was made on |
| [service-user-ids.md](service-user-ids.md) | Why every node of a cluster must use the same db-branch uid/gid, how `cluster join` and `cluster align-ids` align them, and how mismatches are reported |
| [base-refresh.md](base-refresh.md) | Reloading a base from a source database on a schedule |
| [masking.md](masking.md) | Creating masked bases with masker modules and what is recorded about them |
| [modules.md](modules.md) | Writing and deploying extension modules: ports, plugin protocol, settings, module APIs and pages, cluster-wide install |
| [mcp.md](mcp.md) | The MCP server for LLM agents: endpoint, tokens and scopes, tools, connecting Claude Code |
| [monitoring.md](monitoring.md) | Prometheus metrics and health endpoints |
| [notifications.md](notifications.md) | Slack and webhook notifications and the events that trigger them |
| [updates.md](updates.md) | Minor-version cluster updates from the console or CLI |
| [distribution.md](distribution.md) | The apt repository: user steps, contents, release pipeline and signing key handling |
| [license-renewal.md](license-renewal.md) | Monthly and yearly licence keys and how automatic renewal works |
