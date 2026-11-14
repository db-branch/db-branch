// The admin's view of the storage: who uses the space and what deleting would
// free. Pure: it reads a node's storage report together with the lists of bases
// and branches.
import type { SpaceNode, StorageReport } from "../api";

export type BaseRef = { name: string; latest_snapshot: string | null };
export type BranchRef = { name: string; base: string; snapshot: string; expires_at: string | null };

export type SnapshotUsage = { name: string; used: number; created: string; latest: boolean; blockedBy: string[] };
export type BranchUsage = { name: string; base: string; snapshot: string; used: number; checkpoints: number; expired: boolean; expiresAt: string | null; share: number };
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
// TreeNode is one line of the tree: a base, its data, a snapshot, a branch, or the
// group of branches that have no base. total is the node with everything below it.
export type TreeNode = { kind: "base" | "data" | "snapshot" | "branch" | "group"; id: string; name: string; used: number; total: number; share: number; badge?: string; expired?: boolean; children: TreeNode[] };
export type Analysis = {
  pool: { name: string; size: number; free: number; used: number; root: number; other: number };
  bases: BaseUsage[];
  // tree: bases, each with its data and snapshots, each snapshot with the branches cloned from it.
  tree: TreeNode[];
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
      const snapOfBranch = new Map<string, string>();
      for (const sn of snaps) for (const c of sn.clones ?? []) snapOfBranch.set(c, sn.name.slice(ds.name.length + 1));
      const mine = branchDatasets.filter((b) => snaps.some((s) => (s.clones ?? []).includes(b.name)) || branchInfo.get(branchOf(b.name))?.base === name);
      const branchesUsage: BranchUsage[] = mine
        .map((b) => {
          const info = branchInfo.get(branchOf(b.name));
          const exp = info?.expires_at ?? null;
          return { name: branchOf(b.name), base: name, snapshot: snapOfBranch.get(b.name) ?? info?.snapshot ?? "", used: b.used, checkpoints: b.used_by_snapshots ?? 0, expired: !!exp && Date.parse(exp) < now, expiresAt: exp, share: fraction(b.used) };
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
      return { name: branchOf(b.name), base: info?.base ?? fromOrigin, snapshot: info?.snapshot ?? "", used: b.used, checkpoints: b.used_by_snapshots ?? 0, expired: !!exp && Date.parse(exp) < now, expiresAt: exp, share: fraction(b.used) };
    })
    .sort((a, b) => b.used - a.used || a.name.localeCompare(b.name));

  return { pool, bases: usages, tree: treeOf(usages, allBranches, fraction), branches: allBranches, other: { used: otherItems.reduce((s, i) => s + i.used, 0), items: otherItems } };
}

function treeOf(bases: BaseUsage[], all: BranchUsage[], fraction: (n: number) => number): TreeNode[] {
  const node = (kind: TreeNode["kind"], name: string, used: number, children: TreeNode[], extra: Partial<TreeNode> = {}): TreeNode => {
    const total = used + children.reduce((sum, c) => sum + c.total, 0);
    return { kind, id: `${kind}:${name}`, name, used, total, share: fraction(total), children, ...extra };
  };
  const branchNode = (b: BranchUsage) => node("branch", b.name, b.used, [], { expired: b.expired });
  const attached = new Set<string>();
  const roots: TreeNode[] = bases.map((b) => {
    const bySnapshot = new Map<string, BranchUsage[]>();
    const loose: BranchUsage[] = [];
    for (const br of b.branches) {
      attached.add(br.name);
      if (b.snapshots.some((s) => s.name === br.snapshot)) bySnapshot.set(br.snapshot, [...(bySnapshot.get(br.snapshot) ?? []), br]);
      else loose.push(br);
    }
    const snaps = [...b.snapshots]
      .sort((x, y) => Date.parse(y.created) - Date.parse(x.created) || (x.name < y.name ? 1 : x.name > y.name ? -1 : 0))
      .map((s) => node("snapshot", s.name, s.used, (bySnapshot.get(s.name) ?? []).map(branchNode)));
    const data = Math.max(0, b.used - b.snapshotsUsed);
    const children = [...(data > 0 ? [node("data", "data", data, [])] : []), ...snaps, ...loose.map(branchNode)];
    // The base's own use already counts its snapshots; its total adds the branches cloned from it.
    return { kind: "base", id: `base:${b.location}:${b.name}`, name: b.name, used: b.used, total: b.total, share: fraction(b.total), badge: b.location === "replicas" ? "replica" : undefined, children };
  });
  roots.sort((x, y) => y.total - x.total || x.name.localeCompare(y.name));
  const stray = all.filter((b) => !attached.has(b.name));
  if (stray.length > 0) roots.push(node("group", "No base", 0, stray.map(branchNode)));
  return roots;
}
