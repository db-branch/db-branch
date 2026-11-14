import { describe, expect, it } from "vitest";
import type { SpaceNode, StorageReport } from "../api";
import { analyze } from "./storage";

const R = "tank/db-branch";
const at = "2026-11-12T10:00:00Z";
const fs = (name: string, used: number, extra: Partial<SpaceNode> = {}): SpaceNode => ({ name: `${R}/${name}`, kind: "filesystem", used, referenced: used, created: at, ...extra });
const snap = (name: string, used: number, clones: string[] = [], extra: Partial<SpaceNode> = {}): SpaceNode => ({
  name: `${R}/${name}`,
  kind: "snapshot",
  used,
  referenced: 1000,
  clones: clones.map((c) => `${R}/${c}`),
  created: at,
  ...extra,
});

const report = (nodes: SpaceNode[], pool = { size: 10_000, free: 6_000 }): StorageReport => ({ node: "n1", pool: "tank", pool_size: pool.size, pool_free: pool.free, root: R, nodes });

// orders: base 900 (snapshots hold 100), s1 used by branches x and y, s2 free, s3 latest.
// shop: base 300 with one snapshot and no branch. x is the only clone of nothing else; z is a branch of shop? no: shop has none.
const nodes: SpaceNode[] = [
  fs("", 1500),
  fs("bases/orders", 900, { used_by_snapshots: 100 }),
  snap("bases/orders@s1", 60, ["branches/x", "branches/y"]),
  snap("bases/orders@s2", 30),
  snap("bases/orders@s3", 10),
  fs("bases/shop", 300, { used_by_snapshots: 20 }),
  snap("bases/shop@s1", 20),
  fs("branches/x", 50, { origin: `${R}/bases/orders@s1`, used_by_snapshots: 5 }),
  fs("branches/y", 30, { origin: `${R}/bases/orders@s1` }),
  fs("history/x/1", 40),
  fs("work/abc", 5),
];
const bases = [
  { name: "orders", latest_snapshot: "s3" },
  { name: "shop", latest_snapshot: "s1" },
];
const branches = [
  { name: "x", base: "orders", snapshot: "s1", expires_at: "2026-11-10T00:00:00Z" },
  { name: "y", base: "orders", snapshot: "s1", expires_at: null },
];
const now = Date.parse("2026-11-12T12:00:00Z");

describe("analyze: the pool", () => {
  it("splits the pool into what db-branch holds, other use, and free space", () => {
    const a = analyze(report(nodes), bases, branches, now);
    expect(a.pool).toEqual({ name: "tank", size: 10_000, free: 6_000, used: 4_000, root: 1_500, other: 2_500 });
  });
});

describe("analyze: who uses the space", () => {
  const a = analyze(report(nodes), bases, branches, now);
  it("sums a base with the branches cloned from it, biggest first", () => {
    expect(a.bases.map((b) => [b.name, b.total])).toEqual([
      ["orders", 900 + 50 + 30],
      ["shop", 300],
    ]);
    const orders = a.bases[0];
    expect(orders).toMatchObject({ used: 900, snapshotsUsed: 100, branchesUsed: 80, location: "bases" });
    expect(orders.branches.map((b) => [b.name, b.used])).toEqual([["x", 50], ["y", 30]]);
  });
  it("lists the snapshots newest name last as given, with the clones that block them", () => {
    const s = a.bases[0].snapshots;
    expect(s.map((x) => [x.name, x.used, x.latest, x.blockedBy])).toEqual([
      ["s1", 60, false, ["x", "y"]],
      ["s2", 30, false, []],
      ["s3", 10, true, []],
    ]);
  });
  it("counts what is not a base or a branch as other (history of restored lines, work files)", () => {
    expect(a.other).toEqual({ used: 45, items: [{ name: "history/x/1", used: 40 }, { name: "work/abc", used: 5 }] });
  });
  it("marks the branches that expired", () => {
    expect(a.bases[0].branches.find((b) => b.name === "x")?.expired).toBe(true);
    expect(a.bases[0].branches.find((b) => b.name === "y")?.expired).toBe(false);
  });
});

describe("analyze: the branches", () => {
  const a = analyze(report(nodes), bases, branches, now);
  it("lists every branch with its base and the space it uses, biggest first", () => {
    expect(a.branches.map((b) => [b.name, b.base, b.used, b.checkpoints])).toEqual([
      ["x", "orders", 50, 5],
      ["y", "orders", 30, 0],
    ]);
  });
  it("takes the base from the branch list, and shows a branch the list does not know with an empty base", () => {
    const unknown = analyze(report([...nodes, fs("branches/z", 70)]), bases, branches, now);
    expect(unknown.branches[0]).toMatchObject({ name: "z", base: "", used: 70 });
  });
  it("gives the shares: what a base or a branch uses of the pool", () => {
    expect(a.bases[0].share).toBeCloseTo((900 + 80) / 10_000);
    expect(a.branches[0].share).toBeCloseTo(50 / 10_000);
  });
});

describe("analyze: the tree of bases, snapshots and branches", () => {
  const a = analyze(report(nodes), bases, branches, now);
  const shape = (n: { kind: string; name: string; used: number; total: number; children: unknown[] }): unknown => ({ kind: n.kind, name: n.name, used: n.used, total: n.total, children: (n.children as (typeof n)[]).map(shape) });
  it("puts each snapshot under its base and each branch under the snapshot it was cloned from", () => {
    const orders = a.tree.find((n) => n.name === "orders")!;
    expect(shape(orders)).toEqual({
      kind: "base",
      name: "orders",
      used: 900,
      total: 900 + 50 + 30,
      children: [
        { kind: "data", name: "data", used: 800, total: 800, children: [] },
        {
          kind: "snapshot",
          name: "s3",
          used: 10,
          total: 10,
          children: [],
        },
        { kind: "snapshot", name: "s2", used: 30, total: 30, children: [] },
        {
          kind: "snapshot",
          name: "s1",
          used: 60,
          total: 60 + 50 + 30,
          children: [
            { kind: "branch", name: "x", used: 50, total: 50, children: [] },
            { kind: "branch", name: "y", used: 30, total: 30, children: [] },
          ],
        },
      ],
    });
  });
  it("lists the bases biggest first, and a base without snapshots has only its data", () => {
    expect(a.tree.map((n) => [n.name, n.total])).toEqual([["orders", 980], ["shop", 300]]);
    const shop = a.tree.find((n) => n.name === "shop")!;
    expect(shop.children.map((c) => [c.kind, c.name, c.total])).toEqual([["data", "data", 280], ["snapshot", "s1", 20]]);
  });
  it("shows a branch whose snapshot is gone under its base, and one with no base in a group of its own", () => {
    const more = analyze(report([...nodes, fs("branches/lost", 7), fs("branches/stray", 9)]), bases, [...branches, { name: "lost", base: "orders", snapshot: "s9", expires_at: null }], now);
    const orders = more.tree.find((n) => n.name === "orders")!;
    expect(orders.children.some((c) => c.kind === "branch" && c.name === "lost")).toBe(true);
    const stray = more.tree.find((n) => n.kind === "group")!;
    expect(stray).toMatchObject({ name: "No base", used: 0, total: 9 });
    expect(stray.children.map((c) => c.name)).toEqual(["stray"]);
  });
  it("gives every node its share of the pool", () => {
    const orders = a.tree.find((n) => n.name === "orders")!;
    expect(orders.share).toBeCloseTo(980 / 10_000);
  });
});

