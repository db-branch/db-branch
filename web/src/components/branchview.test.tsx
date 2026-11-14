import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Branch } from "../api";
import { BranchView } from "./branchview";

const branch = (extra: Partial<Branch> = {}): Branch => ({
  name: "alice-feature-x",
  status: "ready",
  status_reason: null,
  base: "orders",
  snapshot: "s20261112",
  runtime: "mysql@8.4.13",
  node: "node1",
  created_at: "2026-11-12T10:00:00Z",
  expires_at: "2026-11-19T10:00:00Z",
  reset_at: null,
  storage: { used: 1024, referenced: 2048 },
  connection: { host: "10.0.0.1", port: 40001, user: "alice" },
  proxy: null,
  operation: null,
  owner: "alice",
  id: "alice/feature-x",
  access: "private",
  checkpoints: true,
  ...extra,
});
const noop = () => {};
const actions = { onBack: noop, onExtend: noop, onReset: noop, onRecreate: noop, onDelete: noop, onCopyProxy: noop, onPause: noop, onResume: noop };
const view = (b: Branch, props: Record<string, unknown> = {}) => renderToStaticMarkup(<BranchView b={b} clustered={false} max={2048} {...actions} {...props} />);

describe("BranchView", () => {
  it("shows the name, the status, and the facts of the branch", () => {
    const h = view(branch());
    expect(h).toContain("feature-x");
    expect(h).toContain("orders");
    expect(h).toContain("s20261112");
    expect(h).toContain("40001");
    expect(h).toContain("Back to Branches");
    expect(h).toMatch(/<h1/);
  });
  it("shows the node only in a cluster, and the proxy URL action only when there is a proxy", () => {
    expect(view(branch())).not.toContain("node1");
    expect(renderToStaticMarkup(<BranchView b={branch()} clustered max={2048} {...actions} />)).toContain("10.0.0.1");
    expect(view(branch())).not.toContain("Copy proxy URL");
    expect(view(branch({ proxy: { host: "h", port: 6033, user: "u" } }))).toContain("Copy proxy URL");
  });
  it("offers extend, reset and delete for a ready branch, and recreate only for a lost one", () => {
    const h = view(branch());
    for (const l of ["Extend 1h", "Extend 24h", "Extend 7d", "Reset", "Delete"]) expect(h).toContain(l);
    expect(h).not.toContain("Recreate");
    const lost = view(branch({ status: "lost" }));
    expect(lost).toContain("Recreate");
    expect(lost).toMatch(/<button[^>]*disabled[^>]*>Reset/);
  });
  it("disables reset while the branch is busy, and delete unless it is ready, failed or lost", () => {
    const h = view(branch({ status: "restoring" }));
    expect(h).toMatch(/<button[^>]*disabled[^>]*>Reset/);
    expect(h).toMatch(/<button[^>]*disabled[^>]*>Delete/);
    expect(view(branch({ status: "failed" }))).not.toMatch(/<button[^>]*disabled[^>]*>Delete/);
  });
  it("shows the reason of a failed branch, and the access, recording and owner", () => {
    const h = view(branch({ status: "failed", status_reason: "no space left", access: "open", record: true }));
    expect(h).toContain("no space left");
    expect(h).toContain("open");
    expect(h).toContain("Recording");
    expect(h).toContain("alice");
  });
  it("puts the sections it is given under their titles", () => {
    const h = view(branch(), { sections: [{ id: "x", title: "Connect", body: <p>CONNECT BODY</p> }, { id: "y", title: "Checkpoints", body: <p>CP BODY</p> }] });
    expect(h.indexOf("Connect")).toBeLessThan(h.indexOf("CONNECT BODY"));
    expect(h.indexOf("CONNECT BODY")).toBeLessThan(h.indexOf("CP BODY"));
    expect(h).toContain('id="x"');
  });
  it("offers Pause for a ready branch and Resume for a paused one, never both", () => {
    const ready = view(branch());
    expect(ready).toContain("Pause");
    expect(ready).not.toContain("Resume");
    const paused = view(branch({ status: "paused" }));
    expect(paused).toContain("Resume");
    expect(paused).not.toMatch(/>Pause</);
  });
  it("disables Pause while the branch is busy, and keeps Reset off and Delete on for a paused branch", () => {
    expect(view(branch({ status: "restoring" }))).not.toMatch(/>Pause</);
    const paused = view(branch({ status: "paused" }));
    expect(paused).toMatch(/<button[^>]*disabled[^>]*>Reset/);
    expect(paused).not.toMatch(/<button[^>]*disabled[^>]*>Delete/);
  });
});

