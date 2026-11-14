import { ReactNode } from "react";
import { bytes, relative } from "../lib/format";
import { Analysis, BaseUsage, BranchUsage } from "../lib/storage";

// share is a width in percent, at least a sliver while there is something.
const share = (n: number, of: number) => (of > 0 && n > 0 ? Math.max(1, (n / of) * 100) : 0);
const percent = (f: number) => (f >= 0.1 ? `${Math.round(f * 100)}%` : f > 0 ? `${(f * 100).toFixed(1)}%` : "0%");

function PoolBar({ pool }: { pool: Analysis["pool"] }) {
  const parts: [string, number, string][] = [
    ["db-branch", pool.root, "seg-db"],
    ["Other", pool.other, "seg-other"],
    ["Free", pool.free, "seg-free"],
  ];
  return (
    <div className="pool">
      <div className="pool-head">
        <span className="mono strong">{pool.name}</span>
        <span className="muted mono">{bytes(pool.size)}</span>
      </div>
      <div className="stack" role="img" aria-label={parts.map(([l, v]) => `${l} ${bytes(v)}`).join(", ")}>
        {parts.map(([l, v, cls]) => (
          <span key={l} className={`seg ${cls}`} style={{ width: `${share(v, pool.size)}%` }} title={`${l} ${bytes(v)}`} />
        ))}
      </div>
      <div className="pool-legend small">
        {parts.map(([l, v, cls]) => (
          <span key={l}>
            <i className={`dot ${cls}`} /> {l} <span className="mono">{bytes(v)}</span>
          </span>
        ))}
      </div>
    </div>
  );
}

// StorageView is the Storage page: how much of the pool each base and each branch uses.
export function StorageView({
  analysis: a,
  nodes,
  node,
  onNode,
  onOpen,
}: {
  analysis: Analysis;
  nodes: string[];
  node: string;
  onNode: (n: string) => void;
  onOpen: (href: string) => void;
}): ReactNode {
  const maxBase = Math.max(1, ...a.bases.map((b) => b.total));
  const maxBranch = Math.max(1, ...a.branches.map((b) => b.used));
  const link = (href: string, text: string) => (
    <a className="mono strong" href={href} onClick={(e) => (e.stopPropagation(), e.preventDefault(), onOpen(href))}>
      {text}
    </a>
  );
  return (
    <section className="page storage-page">
      <header className="page-head">
        <h1>Storage</h1>
        {nodes.length > 1 && (
          <select aria-label="Node" value={node} onChange={(e) => onNode(e.target.value)}>
            {nodes.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        )}
      </header>
      <PoolBar pool={a.pool} />

      <h2>
        Bases <span className="muted">{a.bases.length}</span>
      </h2>
      {a.bases.map((b) => (
        <BaseBlock key={`${b.location}:${b.name}`} base={b} max={maxBase} link={link} />
      ))}

      <h2>
        Branches <span className="muted">{a.branches.length}</span>
      </h2>
      {a.branches.length > 0 && (
        <table className="rows compact">
          <thead>
            <tr>
              <th>Name</th>
              <th>Base</th>
              <th>Size</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {a.branches.map((br: BranchUsage) => (
              <tr key={br.name}>
                <td>{link(`/admin/branches/${encodeURIComponent(br.name)}`, br.name)}</td>
                <td className="mono muted">{br.base}</td>
                <td>
                  <span className="bar">
                    <span className="bar-own" style={{ width: `${share(br.used, maxBranch)}%` }} />
                  </span>
                  <span className="mono"> {bytes(br.used)}</span>
                  <span className="muted small"> {percent(br.share)}</span>
                </td>
                <td>{br.expired && <span className="badge warn">Expired</span>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {a.other.used > 0 && (
        <p className="muted small">
          Other <span className="mono">{bytes(a.other.used)}</span> ({a.other.items.length}): {a.other.items.slice(0, 5).map((i) => `${i.name} ${bytes(i.used)}`).join(", ")}
        </p>
      )}
    </section>
  );
}

function BaseBlock({ base: b, max, link }: { base: BaseUsage; max: number; link: (href: string, text: string) => ReactNode }) {
  const own = Math.max(0, b.used - b.snapshotsUsed);
  return (
    <details className="base-block">
      <summary>
        {link(`/admin/bases/${encodeURIComponent(b.name)}`, b.name)}
        {b.location === "replicas" && <span className="badge">replica</span>}
        <span className="stack small-stack" style={{ width: `${share(b.total, max)}%` }} role="img" aria-label={`${bytes(own)} data, ${bytes(b.snapshotsUsed)} snapshots, ${bytes(b.branchesUsed)} branches`}>
          <span className="seg seg-db" style={{ width: `${share(own, b.total)}%` }} title={`Data ${bytes(own)}`} />
          <span className="seg seg-snap" style={{ width: `${share(b.snapshotsUsed, b.total)}%` }} title={`Snapshots ${bytes(b.snapshotsUsed)}`} />
          <span className="seg seg-branch" style={{ width: `${share(b.branchesUsed, b.total)}%` }} title={`Branches ${bytes(b.branchesUsed)}`} />
        </span>
        <span className="mono">{bytes(b.total)}</span>
        <span className="muted small">{percent(b.share)}</span>
      </summary>
      <div className="base-detail small">
        <span className="muted">
          <i className="dot seg-db" /> Data <span className="mono">{bytes(own)}</span>
        </span>
        <span className="muted">
          <i className="dot seg-snap" /> Snapshots <span className="mono">{bytes(b.snapshotsUsed)}</span>
        </span>
        <span className="muted">
          <i className="dot seg-branch" /> Branches <span className="mono">{bytes(b.branchesUsed)}</span>
        </span>
      </div>
      {b.snapshots.length > 0 && (
        <table className="rows compact">
          <thead>
            <tr>
              <th>Snapshot</th>
              <th>Size</th>
              <th>Taken</th>
            </tr>
          </thead>
          <tbody>
            {[...b.snapshots].reverse().map((s) => (
              <tr key={s.name}>
                <td className="mono">{s.name}</td>
                <td className="mono">{bytes(s.used)}</td>
                <td className="muted" title={new Date(s.created).toLocaleString()}>
                  {relative(s.created)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </details>
  );
}
