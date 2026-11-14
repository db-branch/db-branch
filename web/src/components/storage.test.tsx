import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Analysis } from "../lib/storage";
import { StorageView } from "./storage";

const analysis: Analysis = {
  pool: { name: "tank", size: 10 * 1024 ** 3, free: 6 * 1024 ** 3, used: 4 * 1024 ** 3, root: 3 * 1024 ** 3, other: 1024 ** 3 },
  bases: [
    {
      name: "orders",
      location: "bases",
      used: 2 * 1024 ** 3,
      snapshotsUsed: 300 * 1024 ** 2,
      branchesUsed: 100 * 1024 ** 2,
      total: 2 * 1024 ** 3 + 100 * 1024 ** 2,
      snapshots: [
        { name: "s1", used: 200 * 1024 ** 2, created: "2026-11-12T10:00:00Z", latest: false, blockedBy: ["x"] },
        { name: "s2", used: 100 * 1024 ** 2, created: "2026-11-12T11:00:00Z", latest: false, blockedBy: [] },
        { name: "s3", used: 10 * 1024 ** 2, created: "2026-11-12T12:00:00Z", latest: true, blockedBy: [] },
      ],
      branches: [{ name: "x", used: 100 * 1024 ** 2, checkpoints: 0, expired: true, expiresAt: "2026-11-10T00:00:00Z" }],
    },
  ],
  other: { used: 0, items: [] },
  candidates: [
    { kind: "snapshot", id: "orders@s2", base: "orders", label: "orders@s2", freed: 100 * 1024 ** 2 },
    { kind: "branch", id: "x", base: "orders", label: "x", freed: 300 * 1024 ** 2, note: "orders@s1" },
  ],
};
const noop = () => {};
const view = (props: Record<string, unknown> = {}) =>
  renderToStaticMarkup(<StorageView analysis={analysis} nodes={["n1"]} node="n1" onNode={noop} onOpen={noop} onDeleteSnapshot={noop} working={false} {...props} />);

describe("StorageView", () => {
  it("shows the pool: what db-branch holds, other use and free space", () => {
    const h = view();
    expect(h).toContain("tank");
    expect(h).toContain("3.0 GiB");
    expect(h).toContain("6.0 GiB");
    for (const l of ["db-branch", "Other", "Free"]) expect(h).toContain(l);
  });
  it("lists what to delete to free space, with the gain and what else it lets go", () => {
    const h = view();
    expect(h).toContain("Free up space");
    expect(h).toContain("orders@s2");
    expect(h).toContain("100 MiB");
    expect(h).toContain("300 MiB");
    expect(h).toContain("orders@s1"); // the note of branch x
  });
  it("says there is nothing to free when there is no candidate", () => {
    expect(view({ analysis: { ...analysis, candidates: [] } })).toContain("Nothing to free");
  });
  it("shows each base with its total, snapshots with their state, and its branches", () => {
    const h = view();
    expect(h).toContain("orders");
    expect(h).toContain("2.1 GiB");
    expect(h).toContain("Latest"); // s3 is kept
    expect(h).toContain("In use"); // s1 has a clone
    expect(h).toContain("Expired");
  });
  it("offers Delete only for a snapshot that can go", () => {
    const h = view();
    // the latest snapshot and the one a branch is cloned from have no Delete: only s2 (in the table and in the list)
    expect(h.match(/>Delete</g)?.length).toBe(2);
  });
  it("has a node choice only when there are several nodes", () => {
    expect(view()).not.toContain("<select");
    expect(view({ nodes: ["n1", "n2"] })).toContain("<select");
  });
});
