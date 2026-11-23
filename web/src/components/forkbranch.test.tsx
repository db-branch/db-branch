import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Branch } from "../api";
import { ForkBranch } from "./forkbranch";

const b = {
  name: "feature-x",
  status: "ready",
  status_reason: null,
  base: "prod",
  snapshot: "s1",
  runtime: "mysql@8.4.13",
  node: "node1",
  created_at: "2026-11-12T10:00:00Z",
  expires_at: null,
  reset_at: null,
  storage: null,
  connection: { host: "10.0.0.1", port: 40001, user: "feature-x" },
  proxy: null,
  operation: null,
  owner: "alice",
  id: "alice/feature-x",
  access: "private",
  checkpoints: true,
} as Branch;

describe("ForkBranch", () => {
  it("renders the fork form with checkpoint choices and optional owner prefix", () => {
    const h = renderToStaticMarkup(
      <ForkBranch
        branch={b}
        prefix="alice-"
        initialCheckpoint="cp1"
        checkpoints={[{ name: "cp1", created_at: "2026-11-12T10:05:00Z", size: 100, line: 0, auto: false, current: true }]}
        onClose={() => undefined}
      />,
    );
    expect(h).toContain("Fork feature-x");
    expect(h).toContain('class="name-input"');
    expect(h).toContain('class="name-prefix mono"');
    expect(h).toContain("alice-");
    expect(h).toContain("Current state");
    expect(h).toContain("@reset");
    expect(h).toContain("cp1");
  });

  it("includes initialCheckpoint as an option even before checkpoints have loaded", () => {
    const h = renderToStaticMarkup(<ForkBranch branch={b} initialCheckpoint="cp-pending" onClose={() => undefined} />);
    expect(h).toContain('<option value="cp-pending" selected="">cp-pending</option>');
    expect(h).not.toContain("name-prefix");
  });
});
