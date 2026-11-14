// The admin's view of the storage: who uses the space and what deleting would
// free. Pure: it reads a node's storage report together with the lists of bases
// and branches.
import type { SpaceNode, StorageReport } from "../api";

export type BaseRef = { name: string; latest_snapshot: string | null };
export type BranchRef = { name: string; base: string; snapshot: string; expires_at: string | null };

export type SnapshotUsage = { name: string; used: number; created: string; latest: boolean; blockedBy: string[] };
export type BranchUsage = { name: string; used: number; checkpoints: number; expired: boolean; expiresAt: string | null };
export type BaseUsage = {
  name: string;
  // location: "bases" is where the base is kept; "replicas" a copy another node made.
  location: "bases" | "replicas";
  used: number;
  snapshotsUsed: number;
  branchesUsed: number;
  total: number;
  snapshots: SnapshotUsage[];
  branches: BranchUsage[];
};
export type Candidate = { kind: "base" | "branch" | "snapshot"; id: string; base: string; label: string; freed: number; note?: string };
export type Analysis = {
  pool: { name: string; size: number; free: number; used: number; root: number; other: number };
  bases: BaseUsage[];
  other: { used: number; items: { name: string; used: number }[] };
  candidates: Candidate[];
};

const KIND_ORDER = { base: 0, branch: 1, snapshot: 2 } as const;
const rel = (root: string, name: string) => (name.startsWith(root + "/") ? name.slice(root.length + 1) : name);

export function analyze(report: StorageReport, bases: BaseRef[], branches: BranchRef[], now: number): Analysis {
  const root = report.root;
  const byName = new Map(report.nodes.map((n) => [rel(root, n.name), n]));
  const latest = new Map(bases.map((b) => [b.name, b.latest_snapshot]));
  const branchInfo = new Map(branches.map((b) => [b.name, b]));

  const fsOf = (kind: "bases" | "replicas") =>
    report.nodes.filter((n) => n.kind === "filesystem" && rel(root, n.name).startsWith(kind + "/") && rel(root, n.name).split("/").length === 2);
  const branchDatasets = report.nodes.filter((n) => n.kind === "filesystem" && rel(root, n.name).startsWith("branches/") && rel(root, n.name).split("/").length === 2);
  const branchOf = (dataset: string) => rel(root, dataset).split("/")[1];

  const usages: BaseUsage[] = [];
  for (const location of ["bases", "replicas"] as const) {
    for (const ds of fsOf(location)) {
      const name = rel(root, ds.name).split("/")[1];
      const snaps: SpaceNode[] = report.nodes.filter((n) => n.kind === "snapshot" && n.name.startsWith(ds.name + "@"));
      const snapshots: SnapshotUsage[] = snaps.map((s) => ({
        name: s.name.slice(ds.name.length + 1),
        used: s.used,
        created: s.created,
        latest: latest.get(name) === s.name.slice(ds.name.length + 1),
        blockedBy: (s.clones ?? []).map(branchOf),
      }));
      // The branches of this base on this node: cloned from one of its snapshots, or listed under it.
      const mine = branchDatasets.filter((b) => snaps.some((s) => (s.clones ?? []).includes(b.name)) || branchInfo.get(branchOf(b.name))?.base === name);
      const branchesUsage: BranchUsage[] = mine
        .map((b) => {
          const info = branchInfo.get(branchOf(b.name));
          const exp = info?.expires_at ?? null;
          return { name: branchOf(b.name), used: b.used, checkpoints: b.used_by_snapshots ?? 0, expired: !!exp && Date.parse(exp) < now, expiresAt: exp };
        })
        .sort((a, b) => b.used - a.used || a.name.localeCompare(b.name));
      const branchesUsed = branchesUsage.reduce((s, b) => s + b.used, 0);
      usages.push({ name, location, used: ds.used, snapshotsUsed: ds.used_by_snapshots ?? 0, branchesUsed, total: ds.used + branchesUsed, snapshots, branches: branchesUsage });
    }
  }
  usages.sort((a, b) => b.total - a.total || a.name.localeCompare(b.name));

  // What is neither a base nor a branch: the lines a restore set aside, work files, and the like.
  const known = (n: SpaceNode) => {
    const r = rel(root, n.name);
    return n.kind !== "filesystem" || r === root || r === "" || r.startsWith("bases/") || r.startsWith("replicas/") || r.startsWith("branches/") || ["bases", "replicas", "branches"].includes(r);
  };
  const otherItems = report.nodes
    .filter((n) => !known(n) && rel(root, n.name).includes("/"))
    .map((n) => ({ name: rel(root, n.name), used: n.used }))
    .sort((a, b) => b.used - a.used || a.name.localeCompare(b.name));

  const rootUsed = byName.get("")?.used ?? report.nodes.find((n) => n.name === root)?.used ?? 0;
  const used = report.pool_size - report.pool_free;
  const pool = { name: report.pool, size: report.pool_size, free: report.pool_free, used, root: rootUsed, other: Math.max(0, used - rootUsed) };

  return { pool, bases: usages, other: { used: otherItems.reduce((s, i) => s + i.used, 0), items: otherItems }, candidates: candidatesOf(usages) };
}

function candidatesOf(bases: BaseUsage[]): Candidate[] {
  const out: Candidate[] = [];
  for (const b of bases) {
    if (b.branches.length === 0 && b.snapshots.every((s) => s.blockedBy.length === 0)) {
      if (b.used > 0) out.push({ kind: "base", id: b.name, base: b.name, label: b.name, freed: b.used });
    }
    for (const br of b.branches) {
      // The last clone of a snapshot that is not the latest lets that snapshot go too.
      const letGo = b.snapshots.filter((s) => !s.latest && s.blockedBy.length > 0 && s.blockedBy.every((x) => x === br.name));
      const extra = letGo.reduce((sum, s) => sum + s.used, 0);
      const freed = br.used + extra;
      if (freed > 0) {
        out.push({ kind: "branch", id: br.name, base: b.name, label: br.name, freed, note: letGo.length ? letGo.map((s) => `${b.name}@${s.name}`).join(", ") : undefined });
      }
    }
    for (const s of b.snapshots) {
      if (!s.latest && s.blockedBy.length === 0 && s.used > 0) {
        out.push({ kind: "snapshot", id: `${b.name}@${s.name}`, base: b.name, label: `${b.name}@${s.name}`, freed: s.used });
      }
    }
  }
  return out.sort((a, b) => b.freed - a.freed || KIND_ORDER[a.kind] - KIND_ORDER[b.kind] || a.label.localeCompare(b.label));
}
