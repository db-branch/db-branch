import { useState } from "react";
import { osLabel, zfsLabel } from "../lib/system";
import { api, can, ClusterNode, NodeSystem, Operation, Runtime } from "../api";
import { Engine, Icon, Status } from "../components/marks";
import { Sparkline } from "../components/sparkline";
import { Dialog, Drawer, ErrorLine, Info } from "../components/ui";
import { bytes, relative } from "../lib/format";
import { useLive, useNow } from "../lib/live";
import { t } from "../lib/i18n";

// Each node is a column: role, health, pool usage, the branches it holds and
// its engines. Everything is managed from whichever node serves the console.
export function Cluster() {
  const { data: cluster, error } = useLive(api.cluster, ["node", "branch", "runtime"]);
  const { data: branches } = useLive(api.branches, ["branch"]);
  const { data: ops } = useLive(api.operations, ["operation"]);
  useNow(10000);
  // Installs in progress, by node.
  const installing = (ops ?? []).filter((o) => o.kind === "runtime.install" && o.state === "running");

  return (
    <section className="page">
      <header className="page-head">
        <h1>{t("Cluster")}</h1>
        <span className="spacer" />
        {cluster && (
          <span className="muted mono">
            {cluster.mode === "single" ? "single node" : `${cluster.nodes.filter((n) => !n.down).length}/${cluster.nodes.length} up`}
            {cluster.nodes.some((n) => n.protocol > cluster.protocol) && (
              <span className="warn" title={t("Newer features wait until every node is upgraded (cluster protocol {protocol})", { protocol: cluster.protocol })}>
                {" "}· upgrading
              </span>
            )}
          </span>
        )}
      </header>
      <ErrorLine error={error} />
      {cluster?.nodes.some((n) => n.id_problem) && (
        <p className="warn" role="alert">
          {t("Some nodes use other db-branch user ids than the cluster and take no new branches or replicas until they are fixed (db-branch cluster align-ids).")}
        </p>
      )}
      <div className="nodes">
        {cluster?.nodes.map((n) => (
          <NodeColumn
            key={n.name}
            node={n}
            branches={(branches ?? []).filter((b) => b.node === n.name)}
          />
        ))}
      </div>
      {cluster && <Engines nodes={cluster.nodes} installing={installing} />}
    </section>
  );
}

function NodeColumn({ node, branches }: { node: ClusterNode; branches: { name: string; status: string }[] }) {
  const used = node.pool_size - node.pool_free;
  const fill = node.pool_size > 0 ? used / node.pool_size : 0;
  return (
    <article className={`node ${node.down ? "node-down" : ""}`}>
      <header className="node-head">
        <Status status={node.down ? "lost" : "ready"} reason={node.down && node.last_seen ? t("last seen {when}", { when: relative(node.last_seen) }) : null} />
        <span className="mono strong">{node.name}</span>
        {node.leader && (
          <span className="leader" title={t("Leader")}>
            L
          </span>
        )}
        <span className="spacer" />
        <span className={`voting ${node.voter ? "voting-voter" : ""}`}>{node.voter ? t("Voter") : t("Non-voter")}</span>
        <Info label={t("About voters")}>
          {t("Only an odd number of nodes, at most five, vote on cluster changes, so writes wait for few nodes and a split cannot leave two halves. The others keep a full copy and are promoted automatically when a voter is lost.")}
        </Info>
        <span className="mono muted node-host" title={`version ${node.version}`}>
          {node.host}
        </span>
      </header>
      <div className="node-section">
        <h3>{t("Pool")}</h3>
        <span className={`pool ${fill > 0.9 ? "pool-bad" : fill > 0.8 ? "pool-warn" : ""}`} title={t("{used} used of {total}", { used: bytes(used), total: bytes(node.pool_size) })}>
          <span className="bar">
            <span className="bar-own" style={{ width: `${Math.max(1, fill * 100)}%` }} />
          </span>
          <span className="mono">{bytes(node.pool_free)} free</span>
        </span>
      </div>
      {node.arc_max_mb > 0 && (
        <div className="node-section">
          <h3>{t("ZFS ARC max")}</h3>
          <span className="mono">{bytes(node.arc_max_mb * 1024 * 1024)}</span>
          {node.arc_problem && (
            <span className="warn" title={node.arc_problem}>
              {" "}
              {node.arc_problem}
            </span>
          )}
        </div>
      )}
      {node.id_problem && (
        <div className="node-section">
          <h3>{t("db-branch ids")}</h3>
          <span className="mono">{node.service_ids ? `uid ${node.service_ids.uid} / gid ${node.service_ids.gid}` : "—"}</span>
          <span className="warn" role="alert">
            {" "}
            {node.id_problem}
          </span>
        </div>
      )}
      <NodeSystemInfo system={node.system} />
      {!node.down && <NodeMetrics node={node.name} />}
      <div className="node-section">
        <h3>{t("Branches")}</h3>
        <div className="cells">
          {branches.map((b) => (
            <span key={b.name} className="cell" title={`${b.name} · ${b.status}`}>
              <Status status={b.status} />
            </span>
          ))}
          {branches.length === 0 && <span className="muted">—</span>}
        </div>
      </div>
    </article>
  );
}

// NodeSystemInfo shows what the node runs on. Nodes of mixed distributions
// matter for PostgreSQL (docs/mixed-os.md), so the key is shown with the name.
function NodeSystemInfo({ system }: { system?: NodeSystem }) {
  const os = osLabel(system);
  const zfs = zfsLabel(system?.zfs);
  return (
    <div className="node-section">
      <h3>{t("System")}</h3>
      <dl className="kv">
        <dt>{t("OS")}</dt>
        <dd className="mono" title={os.title}>
          {os.text}
        </dd>
        <dt>{t("Kernel")}</dt>
        <dd className="mono">{system?.kernel || "unknown"}</dd>
        <dt>{t("ZFS")}</dt>
        <dd className="mono" title={zfs.title}>
          {zfs.text}
        </dd>
      </dl>
    </div>
  );
}

// NodeMetrics shows the last hour of a node: CPU, memory, network.
function NodeMetrics({ node }: { node: string }) {
  const { data } = useLive(() => api.nodeMetrics(node), ["node"]);
  const list = data ?? [];
  const times = list.map((m) => m.time);
  const total = list.length ? list[list.length - 1].mem_total : 0;
  const rate = (v: number) => `${bytes(v)}/s`;
  return (
    <div className="node-section metrics">
      <h3>{t("Last hour")}</h3>
      <div className="metric">
        <span className="muted">{t("CPU")}</span>
        <Sparkline label={t("CPU")} values={list.map((m) => m.cpu)} times={times} max={100} format={(v) => `${v.toFixed(0)}%`} />
      </div>
      <div className="metric">
        <span className="muted" title={total ? `of ${bytes(total)}` : undefined}>
          {t("Memory")}
        </span>
        <Sparkline label={t("Memory used")} values={list.map((m) => m.mem_used)} times={times} max={total} format={(v) => bytes(v)} />
      </div>
      <div className="metric">
        <span className="muted">{t("Net in")}</span>
        <Sparkline label={t("Network in")} values={list.map((m) => m.net_rx)} times={times} format={rate} />
      </div>
      <div className="metric">
        <span className="muted">{t("Net out")}</span>
        <Sparkline label={t("Network out")} values={list.map((m) => m.net_tx)} times={times} format={rate} />
      </div>
    </div>
  );
}

// Engines lists every engine version against every node, so it is plain
// where each one is, what uses it, and what can be added. Installing and
// removing act on the whole cluster.
function Engines({ nodes, installing }: { nodes: ClusterNode[]; installing: Operation[] }) {
  const { data: available } = useLive(api.availableRuntimes, ["runtime"]);
  const { data: bases } = useLive(api.bases, ["base"]);
  const { data: branches } = useLive(api.branches, ["branch"]);
  const [adding, setAdding] = useState(false);
  const [removing, setRemoving] = useState<string | null>(null);
  const [error, setError] = useState<unknown>(null);
  const pending = (rt: string, node: string) => installing.some((o) => o.target.name === rt && (o.node ?? node) === node);
  const installed = [...new Set(nodes.flatMap((n) => n.runtimes))];
  const busy = [...new Set(installing.map((o) => o.target.name))].filter((r) => !installed.includes(r));
  const rows = [...installed, ...busy].sort();
  const uses = (rt: string) => (bases ?? []).filter((b) => b.runtime === rt).length + (branches ?? []).filter((b) => b.runtime === rt).length;
  const install = (rt: string) => api.installRuntime(rt).then(() => setError(null), (e: unknown) => setError(e));
  const addable = (available ?? []).filter((r) => !nodes.every((n) => n.runtimes.includes(r.runtime)));

  return (
    <section className="engines">
      <header className="section-head">
        <h2>
          Engines{" "}
          <Info label={t("About engines")}>
            {t("An engine version (e.g. MySQL 8.4) must be installed before bases can use it. Installing puts it on every node, so branches can start anywhere. An engine in use by a base or branch cannot be removed.")}
          </Info>
        </h2>
        {can("runtime.install") && (
          <button onClick={() => setAdding(true)} disabled={addable.length === 0} title={addable.length === 0 ? t("Every available version is installed") : undefined}>
            <Icon name="plus" /> {t("Add engine")}
          </button>
        )}
      </header>
      <ErrorLine error={error} />
      {rows.length === 0 ? (
        <p className="muted">{t("No engines yet. Add one to create bases.")}</p>
      ) : (
        <div className="table-wrap">
          <table className="rows">
            <thead>
              <tr>
                <th>{t("Engine")}</th>
                {nodes.map((n) => (
                  <th key={n.name} className="mono">
                    {n.name}
                  </th>
                ))}
                <th>{t("Used by")}</th>
                <th aria-label={t("Actions")} />
              </tr>
            </thead>
            <tbody>
              {rows.map((rt) => {
                const missing = nodes.filter((n) => !n.runtimes.includes(rt) && !pending(rt, n.name) && !n.down);
                const used = uses(rt);
                return (
                  <tr key={rt}>
                    <td>
                      <Engine runtime={rt} />
                    </td>
                    {nodes.map((n) => (
                      <td key={n.name}>
                        {n.runtimes.includes(rt) ? (
                          <span className="ok" title={t("Installed")}>
                            ✓
                          </span>
                        ) : pending(rt, n.name) ? (
                          <span className="muted">
                            <Status status="provisioning" /> installing
                          </span>
                        ) : (
                          <span className="muted" title={n.down ? t("Node is down") : t("Not installed")}>
                            —
                          </span>
                        )}
                      </td>
                    ))}
                    <td className="muted">{used === 0 ? "—" : `${used} base/branch${used > 1 ? "es" : ""}`}</td>
                    <td className="col-actions">
                      {can("runtime.install") && missing.length > 0 && (
                        <button className="text-button" onClick={() => install(rt)} title={`Install on ${missing.map((n) => n.name).join(", ")}`}>
                          Install on {missing.length === nodes.length ? "all" : `${missing.length} missing`}
                        </button>
                      )}
                      {can("runtime.install") && missing.length === 0 && used === 0 && !busy.includes(rt) && (
                        <button className="text-button danger" onClick={() => setRemoving(rt)}>
                          {t("Remove")}
                        </button>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {adding && <AddEngine available={addable} onClose={() => setAdding(false)} onInstall={(rt) => install(rt).then(() => setAdding(false))} />}
      {removing && (
        <Dialog title={t("Remove {name}?", { name: removing })} onClose={() => setRemoving(null)}>
          <p>{t("It is uninstalled from every node. Nothing uses it now; you can install it again later.")}</p>
          <div className="form-actions">
            <button onClick={() => setRemoving(null)}>{t("Cancel")}</button>
            <button
              className="danger-button"
              onClick={() =>
                api.removeRuntime(removing).then(
                  () => (setRemoving(null), setError(null)),
                  (e: unknown) => (setRemoving(null), setError(e)),
                )
              }
            >
              {t("Remove")}
            </button>
          </div>
        </Dialog>
      )}
    </section>
  );
}

// AddEngine offers the versions not yet everywhere, grouped by engine.
function AddEngine({ available, onClose, onInstall }: { available: Runtime[]; onClose: () => void; onInstall: (rt: string) => void }) {
  const engines = [...new Set(available.map((r) => r.engine))];
  return (
    <Drawer title={t("Add engine")} onClose={onClose}>
      <p className="muted">{t("Installs on every node. Takes a minute or two; progress shows in the Engines table.")}</p>
      {engines.map((e) => (
        <div key={e} className="engine-group">
          <h3>{e === "mysql" ? t("MySQL") : e === "postgres" ? t("PostgreSQL") : e}</h3>
          <ul className="plain-list">
            {available
              .filter((r) => r.engine === e)
              .map((r) => (
                <li key={r.runtime} className="engine-choice">
                  <Engine runtime={r.runtime} />
                  <span className="spacer" />
                  <button onClick={() => onInstall(r.runtime)}>{t("Install")}</button>
                </li>
              ))}
          </ul>
        </div>
      ))}
    </Drawer>
  );
}
