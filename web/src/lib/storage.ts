// The admin's view of the storage: who uses the space and what deleting would
// free. Pure: it reads a node's storage report together with the lists of bases
// and branches.
import type { SpaceNode, StorageReport } from "../api";

export type BaseRef = { name: string; latest_snapshot: string | null };
export type BranchRef = { name: string; base: string; snapshot: string; expires_at: string | null };

export type SnapshotUsage = { name: string; used: number; created: string; latest: boolean; blockedBy: string[] };
export type BranchUsage = { name: string; base: string; used: number; checkpoints: number; expired: boolean; expiresAt: string | null; share: number };
export type BaseUsage = {
  name: string;
  // location: "bases" is where the base is kept; "replicas" a copy another node made.
  location: "bases" | "replicas";
  used: number;
  snapshotsUsed: number;
  branchesUsed: number;
  total: number;
  // share is the part of the pool the base and its branches use.
  share: number;
  snapshots: SnapshotUsage[];
  branches: BranchUsage[];
};
export type Analysis = {
  pool: { name: string; size: number; free: number; used: number; root: number; other: number };
  bases: BaseUsage[];
  // branches are all the branches of the node, the biggest first.
  branches: BranchUsage[];
  other: { used: number; items: { name: string; used: number }[] };
};

const rel = (root: string, name: string) => (name.startsWith(root + "/") ? name.slice(root.length + 1) : name);

export function analyze(report: StorageReport, bases: BaseRef[], branches: BranchRef[], now: number): Analysis {
  const root = report.root;
  const byName = new Map(report.nodes.map((n) => [rel(root, n.name), n]));
  const latest = new Map(bases.map((b) => [b.name, b.latest_snapshot]));
  const branchInfo = new Map(branches.map((b) => [b.name, b]));
  const fraction = (n: number) => (report.pool_size > 0 ? n / report.pool_size : 0);

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
          return { name: branchOf(b.name), base: name, used: b.used, checkpoints: b.used_by_snapshots ?? 0, expired: !!exp && Date.parse(exp) < now, expiresAt: exp, share: fraction(b.used) };
        })
        .sort((a, b) => b.used - a.used || a.name.localeCompare(b.name));
      const branchesUsed = branchesUsage.reduce((s, b) => s + b.used, 0);
      usages.push({ name, location, used: ds.used, snapshotsUsed: ds.used_by_snapshots ?? 0, branchesUsed, total: ds.used + branchesUsed, share: fraction(ds.used + branchesUsed), snapshots, branches: branchesUsage });
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

  const allBranches: BranchUsage[] = branchDatasets
    .map((b) => {
      const info = branchInfo.get(branchOf(b.name));
      const exp = info?.expires_at ?? null;
      const fromOrigin = /\/bases\/([^/@]+)@/.exec(b.origin ?? "")?.[1] ?? "";
      return { name: branchOf(b.name), base: info?.base ?? fromOrigin, used: b.used, checkpoints: b.used_by_snapshots ?? 0, expired: !!exp && Date.parse(exp) < now, expiresAt: exp, share: fraction(b.used) };
    })
    .sort((a, b) => b.used - a.used || a.name.localeCompare(b.name));

  return { pool, bases: usages, branches: allBranches, other: { used: otherItems.reduce((s, i) => s + i.used, 0), items: otherItems } };
}
