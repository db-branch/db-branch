import { ReactNode, useState } from "react";
import { bytes, relative } from "../lib/format";
import { Analysis, BaseUsage, Candidate } from "../lib/storage";
import { t } from "../lib/i18n";

// share is a width in percent, at least a sliver while there is something.
const share = (n: number, of: number) => (of > 0 && n > 0 ? Math.max(1, (n / of) * 100) : 0);

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

// StorageView is the Storage page: the pool, what to delete to free space, and
// who uses it, base by base. Deleting is only offered for a snapshot nothing depends on;
// a base or a branch is deleted on its own page.
export function StorageView({
  analysis: a,
  nodes,
  node,
  onNode,
  onOpen,
  onDeleteSnapshot,
  working,
}: {
  analysis: Analysis;
  nodes: string[];
  node: string;
  onNode: (n: string) => void;
  onOpen: (href: string) => void;
  onDeleteSnapshot: (base: string, snapshot: string) => void;
  working: boolean;
}): ReactNode {
  const [confirming, setConfirming] = useState<string | null>(null);
  const maxTotal = Math.max(1, ...a.bases.map((b) => b.total));
  const del = (c: { base: string; snapshot: string; id: string }) => (
    <button
      type="button"
      className="text-button danger"
      disabled={working}
      onClick={() => (confirming === c.id ? (setConfirming(null), onDeleteSnapshot(c.base, c.snapshot)) : setConfirming(c.id))}
      onBlur={() => setConfirming(null)}
    >
      {confirming === c.id ? t("Really delete?") : "Delete"}
    </button>
  );
  const open = (c: Candidate) =>
    c.kind === "base" ? `/admin/bases/${encodeURIComponent(c.base)}` : c.kind === "branch" ? `/admin/branches/${encodeURIComponent(c.id)}` : `/admin/bases/${encodeURIComponent(c.base)}`;
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

      <h2>Free up space</h2>
      {a.candidates.length === 0 ? (
        <p className="muted">{t("Nothing to free")}</p>
      ) : (
        <table className="rows compact">
          <thead>
            <tr>
              <th>Kind</th>
              <th>Name</th>
              <th>Frees</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {a.candidates.map((c) => (
              <tr key={`${c.kind}:${c.id}`}>
                <td className="muted">{c.kind}</td>
                <td>
                  <span className="mono strong">{c.label}</span>
                  {c.note && <span className="muted small"> + {c.note}</span>}
                </td>
                <td>
                  <span className="bar">
                    <span className="bar-own" style={{ width: `${share(c.freed, a.candidates[0].freed)}%` }} />
                  </span>
                  <span className="mono"> {bytes(c.freed)}</span>
                </td>
                <td className="col-actions">
                  {c.kind === "snapshot" ? (
                    del({ base: c.base, snapshot: c.label.split("@")[1], id: c.id })
                  ) : (
                    <a className="text-button" href={open(c)} onClick={(e) => (e.preventDefault(), onOpen(open(c)))}>
                      Open
                    </a>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h2>
        Bases <span className="muted">{a.bases.length}</span>
      </h2>
      {a.bases.map((b) => (
        <BaseBlock key={`${b.location}:${b.name}`} base={b} max={maxTotal} del={del} onOpen={onOpen} />
      ))}
      {a.other.used > 0 && (
        <p className="muted small">
          Other <span className="mono">{bytes(a.other.used)}</span> ({a.other.items.length}): {a.other.items.slice(0, 5).map((i) => `${i.name} ${bytes(i.used)}`).join(", ")}
        </p>
      )}
    </section>
  );
}

function BaseBlock({ base: b, max, del, onOpen }: { base: BaseUsage; max: number; del: (c: { base: string; snapshot: string; id: string }) => ReactNode; onOpen: (href: string) => void }) {
  const own = Math.max(0, b.used - b.snapshotsUsed);
  const href = `/admin/bases/${encodeURIComponent(b.name)}`;
  return (
    <details className="base-block">
      <summary>
        <a className="mono strong" href={href} onClick={(e) => (e.stopPropagation(), e.preventDefault(), onOpen(href))}>
          {b.name}
        </a>
        {b.location === "replicas" && <span className="badge">replica</span>}
        <span className="stack small-stack" style={{ width: `${share(b.total, max)}%` }} role="img" aria-label={`${bytes(own)} data, ${bytes(b.snapshotsUsed)} snapshots, ${bytes(b.branchesUsed)} branches`}>
          <span className="seg seg-db" style={{ width: `${share(own, b.total)}%` }} title={`Data ${bytes(own)}`} />
          <span className="seg seg-snap" style={{ width: `${share(b.snapshotsUsed, b.total)}%` }} title={`Snapshots ${bytes(b.snapshotsUsed)}`} />
          <span className="seg seg-branch" style={{ width: `${share(b.branchesUsed, b.total)}%` }} title={`Branches ${bytes(b.branchesUsed)}`} />
        </span>
        <span className="mono">{bytes(b.total)}</span>
      </summary>
      <table className="rows compact">
        <thead>
          <tr>
            <th>Snapshot</th>
            <th>Frees</th>
            <th>Taken</th>
            <th>State</th>
            <th />
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
              <td>{s.latest ? <span className="badge">Latest</span> : s.blockedBy.length > 0 ? <span className="muted" title={s.blockedBy.join(", ")}>In use ({s.blockedBy.length})</span> : <span className="muted">Unused</span>}</td>
              <td className="col-actions">{!s.latest && s.blockedBy.length === 0 ? del({ base: b.name, snapshot: s.name, id: `${b.name}@${s.name}` }) : null}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {b.branches.length > 0 && (
        <table className="rows compact">
          <thead>
            <tr>
              <th>Branch</th>
              <th>Own</th>
              <th>Checkpoints</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {b.branches.map((br) => (
              <tr key={br.name}>
                <td>
                  <a className="mono" href={`/admin/branches/${encodeURIComponent(br.name)}`} onClick={(e) => (e.preventDefault(), onOpen(`/admin/branches/${encodeURIComponent(br.name)}`))}>
                    {br.name}
                  </a>
                </td>
                <td className="mono">{bytes(br.used)}</td>
                <td className="mono">{br.checkpoints ? bytes(br.checkpoints) : "—"}</td>
                <td>{br.expired && <span className="badge warn">Expired</span>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </details>
  );
}
