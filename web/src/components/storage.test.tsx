import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Analysis, TreeNode } from "../lib/storage";
import { StorageView } from "./storage";

const MIB = 1024 ** 2;
const n = (kind: TreeNode["kind"], name: string, used: number, children: TreeNode[] = [], extra: Partial<TreeNode> = {}): TreeNode => {
  const total = used + children.reduce((s, c) => s + c.total, 0);
  return { kind, id: `${kind}:${name}`, name, used, total, share: total / (10 * 1024 ** 3), children, ...extra };
};
const orders = n("base", "orders", 2 * 1024 ** 3, [
  n("data", "data", 1.7 * 1024 ** 3),
  n("snapshot", "s3", 10 * MIB),
  n("snapshot", "s1", 200 * MIB, [n("branch", "x", 100 * MIB, [], { expired: true }), n("branch", "y", 5 * MIB)]),
]);
const shop = n("base", "shop", 300 * MIB, [n("data", "data", 300 * MIB)], { badge: "replica" });
const analysis: Analysis = {
  pool: { name: "tank", size: 10 * 1024 ** 3, free: 6 * 1024 ** 3, used: 4 * 1024 ** 3, root: 3 * 1024 ** 3, other: 1024 ** 3 },
  bases: [],
  branches: [],
  tree: [{ ...orders, total: 2 * 1024 ** 3 + 105 * MIB }, shop, n("group", "No base", 0, [n("branch", "stray", 9 * MIB)])],
  other: { used: 0, items: [] },
};
const noop = () => {};
const view = (props: Record<string, unknown> = {}) => renderToStaticMarkup(<StorageView analysis={analysis} nodes={["n1"]} node="n1" onNode={noop} onOpen={noop} {...props} />);

describe("StorageView", () => {
  it("shows the pool: what db-branch holds, other use and free space", () => {
    const h = view();
    expect(h).toContain("tank");
    expect(h).toContain("3.0 GiB");
    expect(h).toContain("6.0 GiB");
    for (const l of ["db-branch", "Other", "Free"]) expect(h).toContain(l);
  });
  it("draws bases, their snapshots and the branches under a snapshot as a tree", () => {
    const h = view();
    expect(h).toMatch(/role="tree"/);
    expect(h.match(/role="treeitem"/g)!.length).toBeGreaterThanOrEqual(9);
    // a branch comes after its snapshot, a snapshot after its base
    expect(h.indexOf("orders")).toBeLessThan(h.indexOf(">s1<"));
    expect(h.indexOf(">s1<")).toBeLessThan(h.indexOf(">x<"));
    expect(h.indexOf(">x<")).toBeLessThan(h.indexOf(">y<"));
  });
  it("tells the kind of each line and shows its size and its share of the pool", () => {
    const h = view();
    for (const k of ["base", "snapshot", "branch", "data"]) expect(h).toContain(`data-kind="${k}"`);
    expect(h).toContain("Collapse all");
    expect(h).toContain("Relative use");
    expect(h).toContain("2.1 GiB");
    expect(h).toContain("200 MiB");
    expect(h).toContain("100 MiB");
    expect(h).toMatch(/\d+(\.\d)?%/);
  });
  it("nests the levels with aria-level, marks replicas and expired branches, and groups branches that have no base", () => {
    const h = view();
    expect(h).toContain('aria-level="1"');
    expect(h).toContain('aria-level="3"');
    expect(h).toContain("replica");
    expect(h).toContain("Expired");
    expect(h).toContain("No base");
    expect(h).toContain("stray");
  });
  it("links a base and a branch to their pages", () => {
    const h = view();
    expect(h).toContain('href="/admin/bases/orders"');
    expect(h).toContain('href="/admin/branches/x"');
  });
  it("has no action and no advice: only sizes", () => {
    const h = view();
    for (const gone of ["Free up space", "Frees", "Delete", "Nothing to free"]) expect(h).not.toContain(gone);
  });
  it("has a node choice only when there are several nodes", () => {
    expect(view()).not.toContain("<select");
    expect(view({ nodes: ["n1", "n2"] })).toContain("<select");
  });
});
