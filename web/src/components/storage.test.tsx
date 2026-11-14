import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Analysis } from "../lib/storage";
import { StorageView } from "./storage";

const MIB = 1024 ** 2;
const analysis: Analysis = {
  pool: { name: "tank", size: 10 * 1024 ** 3, free: 6 * 1024 ** 3, used: 4 * 1024 ** 3, root: 3 * 1024 ** 3, other: 1024 ** 3 },
  bases: [
    {
      name: "orders",
      location: "bases",
      used: 2 * 1024 ** 3,
      snapshotsUsed: 300 * MIB,
      branchesUsed: 100 * MIB,
      total: 2 * 1024 ** 3 + 100 * MIB,
      share: 0.21,
      snapshots: [
        { name: "s1", used: 200 * MIB, created: "2026-11-12T10:00:00Z", latest: false, blockedBy: ["x"] },
        { name: "s3", used: 10 * MIB, created: "2026-11-12T12:00:00Z", latest: true, blockedBy: [] },
      ],
      branches: [{ name: "x", base: "orders", used: 100 * MIB, checkpoints: 0, expired: true, expiresAt: "2026-11-10T00:00:00Z", share: 0.01 }],
    },
    { name: "shop", location: "replicas", used: 300 * MIB, snapshotsUsed: 0, branchesUsed: 0, total: 300 * MIB, share: 0.03, snapshots: [], branches: [] },
  ],
  branches: [
    { name: "x", base: "orders", used: 100 * MIB, checkpoints: 20 * MIB, expired: true, expiresAt: "2026-11-10T00:00:00Z", share: 0.01 },
    { name: "y", base: "shop", used: 5 * MIB, checkpoints: 0, expired: false, expiresAt: null, share: 0.0005 },
  ],
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
  it("lists the bases with the space each uses, biggest first, and the replicas marked", () => {
    const h = view();
    expect(h).toContain("Bases");
    expect(h.indexOf("orders")).toBeLessThan(h.indexOf("shop"));
    expect(h).toContain("2.1 GiB");
    expect(h).toContain("300 MiB");
    expect(h).toContain("replica");
  });
  it("lists every branch with its base and its size, and marks one that expired", () => {
    const h = view();
    expect(h).toContain("Branches");
    expect(h).toContain("100 MiB");
    expect(h).toContain("5.0 MiB");
    expect(h).toContain("Expired");
  });
  it("opens a base to show its snapshots and their sizes, without any state or action", () => {
    const h = view();
    expect(h).toContain("200 MiB");
    for (const gone of ["Free up space", "Frees", "Delete", "Latest", "In use", "Unused", "Nothing to free"]) expect(h).not.toContain(gone);
  });
  it("has a node choice only when there are several nodes", () => {
    expect(view()).not.toContain("<select");
    expect(view({ nodes: ["n1", "n2"] })).toContain("<select");
  });
});
