import { isValidElement, type ReactElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import type { Branch } from "../api";
import { BranchView } from "./branchview";

function findButton(node: ReactNode, label: string): ReactElement<{ onClick?: () => void }> | undefined {
  if (!node || typeof node !== "object") return undefined;
  if (Array.isArray(node)) {
    for (const item of node) {
      const hit = findButton(item, label);
      if (hit) return hit;
    }
    return undefined;
  }
  if (!isValidElement(node)) return undefined;
  const props = node.props as { children?: ReactNode; onClick?: () => void };
  if (node.type === "button" && props.children === label) return node as ReactElement<{ onClick?: () => void }>;
  return findButton(props.children, label);
}

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
  it("shows the node only in a cluster, and the proxy URL action only for MySQL branches with a proxy", () => {
    expect(view(branch())).not.toContain("node1");
    expect(renderToStaticMarkup(<BranchView b={branch()} clustered max={2048} {...actions} />)).toContain("10.0.0.1");
    expect(view(branch())).not.toContain("Copy proxy URL");
    expect(view(branch({ proxy: { host: "h", port: 6033, user: "u" } }))).toContain("Copy proxy URL");
    expect(view(branch({ runtime: "postgres@18.6", proxy: { host: "h", port: 6432, user: "u" } }))).not.toContain("Copy proxy URL");
  });
  it("groups title, status, and badges in page-head-main so status stays beside the branch name", () => {
    const h = view(branch({ record: true }));
    expect(h).toContain('class="page-head-main"');
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
  it("shows owner_name when distinct from immutable owner id", () => {
    const h = view(branch({ owner: "sso:alice", owner_name: "alice@example.com" }));
    expect(h).toContain("alice@example.com");
    expect(h).toContain('title="sso:alice"');
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
  it("shows Fork for ready or paused branches with checkpoints, shows parent link, and hides Recreate on lost forks", () => {
    expect(view(branch(), { onFork: noop })).toContain(">Fork<");
    expect(view(branch({ status: "paused" }), { onFork: noop })).toContain(">Fork<");
    expect(view(branch({ checkpoints: false }), { onFork: noop })).not.toContain(">Fork<");
    expect(view(branch({ status: "restoring" }), { onFork: noop })).not.toContain(">Fork<");
    const forked = view(branch({ parent_branch: "parent-br", parent_snapshot: "fork-alice-feature-x" }), { onOpenParent: noop });
    expect(forked).toContain("Forked from");
    expect(forked).toContain("parent-br");
    const lostFork = view(branch({ status: "lost", parent_branch: "parent-br" }));
    expect(lostFork).not.toContain("Recreate");
  });
});

describe("TestBranchView_Extend1hが残り72hを短縮せず加算する", () => {
  it("adds 1h, 24h, and 7d to the remaining 72h TTL instead of overwriting it", () => {
    vi.useFakeTimers();
    try {
      const now = Date.parse("2026-11-12T10:00:00Z");
      vi.setSystemTime(now);
      const b = branch({ expires_at: new Date(now + 72 * 3600_000).toISOString() });
      const extended: string[] = [];
      const tree = BranchView({ b, clustered: false, max: 2048, ...actions, onExtend: (ttl) => extended.push(ttl) });

      findButton(tree, "Extend 1h")?.props.onClick?.();
      findButton(tree, "Extend 24h")?.props.onClick?.();
      findButton(tree, "Extend 7d")?.props.onClick?.();

      expect(extended).toEqual([`${72 * 60 + 60}m`, `${72 * 60 + 24 * 60}m`, `${72 * 60 + 168 * 60}m`]);
    } finally {
      vi.useRealTimers();
    }
  });
});

