import { ReactNode, useState } from "react";
import { bytes } from "../lib/format";
import { t } from "../lib/i18n";
import { Analysis, TreeNode } from "../lib/storage";

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
    <div className="storage-pool">
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

const expandableKeys = (nodes: TreeNode[], prefix = ""): string[] =>
  nodes.flatMap((n) => {
    const key = prefix ? `${prefix}/${n.id}` : n.id;
    return n.children.length > 0 ? [key, ...expandableKeys(n.children, key)] : [];
  });

// StorageView is the Storage page: the pool, and a tree of the bases, their
// snapshots and the branches cloned from them, each with the space it uses.
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
  const [closed, setClosed] = useState<Set<string>>(() => new Set());
  const max = Math.max(1, ...a.tree.map((n) => n.total));
  const expandable = expandableKeys(a.tree);
  const allClosed = expandable.length > 0 && expandable.every((id) => closed.has(id));
  const toggleAll = () => setClosed(allClosed ? new Set() : new Set(expandable));
  const toggleNode = (id: string) =>
    setClosed((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  return (
    <section className="page storage-page">
      <header className="page-head">
        <h1>Storage</h1>
        <div className="storage-controls">
          {expandable.length > 0 && (
            <button type="button" className="small" onClick={toggleAll}>
              {allClosed ? "Expand all" : "Collapse all"}
            </button>
          )}
          {nodes.length > 1 && (
            <select aria-label="Node" value={node} onChange={(e) => onNode(e.target.value)}>
              {nodes.map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </select>
          )}
        </div>
      </header>
      <PoolBar pool={a.pool} />
      <div className="storage-card">
        <div className="tree-head" aria-hidden="true">
          <span>Item</span>
          <span>Relative use</span>
          <span className="num">Size</span>
          <span className="num">Pool</span>
        </div>
        <ul className="tree" role="tree" aria-label="Storage">
          {a.tree.map((n) => (
            <TreeRow key={n.id} node={n} path="" level={1} max={max} closed={closed} onToggle={toggleNode} onOpen={onOpen} />
          ))}
        </ul>
      </div>
      {a.other.used > 0 && (
        <p className="muted small">
          Other <span className="mono">{bytes(a.other.used)}</span> ({a.other.items.length}): {a.other.items.slice(0, 5).map((i) => `${i.name} ${bytes(i.used)}`).join(", ")}
        </p>
      )}
    </section>
  );
}

const hrefOf = (n: TreeNode): string | null =>
  n.kind === "base" ? `/admin/bases/${encodeURIComponent(n.name)}` : n.kind === "branch" ? `/admin/branches/${encodeURIComponent(n.name)}` : null;

// TreeRow is one line of the tree and, while open, its children. A base, and a
// snapshot with branches under it, start open unless present in closed.
function TreeRow({
  node: n,
  path,
  level,
  max,
  closed,
  onToggle,
  onOpen,
}: {
  node: TreeNode;
  path: string;
  level: number;
  max: number;
  closed: Set<string>;
  onToggle: (id: string) => void;
  onOpen: (href: string) => void;
}) {
  const has = n.children.length > 0;
  const nodeKey = path ? `${path}/${n.id}` : n.id;
  const open = !closed.has(nodeKey);
  const href = hrefOf(n);
  const hideName = n.kind === "data" && n.name === "data";
  return (
    <li role="treeitem" aria-level={level} aria-expanded={has ? open : undefined} data-kind={n.kind} className="tree-item">
      <div className="tree-row">
        <div className="tree-label" style={{ paddingInlineStart: `${(level - 1) * 20}px` }}>
          {has ? (
            <button type="button" className="tree-toggle" aria-label={open ? "Collapse" : "Expand"} onClick={() => onToggle(nodeKey)}>
              {open ? "▾" : "▸"}
            </button>
          ) : (
            <span className="tree-toggle" />
          )}
          <span className={`tree-kind kind-${n.kind}`}>{n.kind === "group" ? "other" : n.kind}</span>
          {!hideName &&
            (href ? (
              <a className="tree-name mono strong" href={href} onClick={(e) => (e.preventDefault(), onOpen(href))}>
                {n.name}
              </a>
            ) : (
              <span className={n.kind === "snapshot" || n.kind === "group" ? "tree-name mono strong" : "tree-name mono muted"}>{n.name}</span>
            ))}
          {n.badge && <span className="badge">{n.badge}</span>}
          {n.expired && <span className="badge warn">Expired</span>}
          {n.kind === "snapshot" && has && <span className="tree-note muted small mono">own {bytes(n.used)}</span>}
        </div>
        <span className="tree-bar bar">
          <span className={`bar-own bar-${n.kind}${n.expired ? " is-expired" : ""}`} style={{ width: `${share(n.total, max)}%` }} />
        </span>
        <span className="mono tree-size" title={has ? t("{size} its own", { size: bytes(n.used) }) : undefined}>
          {bytes(n.total)}
        </span>
        <span className="muted small mono tree-pct">{percent(n.share)}</span>
      </div>
      {has && open && (
        <ul role="group" className="tree-children">
          {n.children.map((c) => (
            <TreeRow key={c.id} node={c} path={nodeKey} level={level + 1} max={max} closed={closed} onToggle={onToggle} onOpen={onOpen} />
          ))}
        </ul>
      )}
    </li>
  );
}
