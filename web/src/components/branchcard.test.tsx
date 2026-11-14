import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Branch } from "../api";
import { BranchRow } from "./branchcard";

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
const row = (props: Record<string, unknown> = {}, b: Branch = branch()) =>
  renderToStaticMarkup(<BranchRow branch={b} now={Date.parse("2026-11-13T10:00:00Z")} manage can={{ "branch.reset": true, "branch.delete": true, "branch.update": true }} mode="list" href="/branch/alice-feature-x" onOpen={noop} onChange={noop} {...props} />);

describe("BranchRow in the list", () => {
  it("is a summary that links to the branch's page, with no details to expand", () => {
    const h = row();
    expect(h).toContain('href="/branch/alice-feature-x"');
    expect(h).toContain("feature-x");
    expect(h).toContain("orders");
    expect(h).not.toContain("brow-details");
    expect(h).not.toContain("aria-expanded");
    expect(h).not.toContain("Snapshot");
  });
  it("keeps the quick actions: copy the URL and extend", () => {
    const h = row();
    expect(h).toContain("Copy URL");
    expect(h).toContain("Extend +4h");
  });
});

describe("BranchRow as the page of the branch", () => {
  it("shows the details at once: facts, connection area, and the keep and reset and delete controls", () => {
    const h = row({ mode: "page" });
    expect(h).toContain("brow-details");
    for (const l of ["Snapshot", "Runtime", "Node", "Owner", "Created", "Expires", "Storage"]) expect(h).toContain(l);
    expect(h).toContain("Reset");
    expect(h).toContain("Delete");
    expect(h).not.toContain("aria-expanded");
    expect(h).not.toContain('href="/branch/alice-feature-x"');
  });
  it("shows the transactions section only when it is given", () => {
    expect(row({ mode: "page" })).not.toContain("Transactions");
    const h = row({ mode: "page", extra: <p>TX BODY</p> });
    expect(h).toContain("TX BODY");
  });
  it("tells a person who is not signed in to sign in, and shows no controls", () => {
    const h = row({ mode: "page", manage: false });
    expect(h).toContain("Sign in to extend, reset or delete.");
  });
});
