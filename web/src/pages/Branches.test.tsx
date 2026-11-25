import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Branch } from "../api";
import { BranchesTable } from "./Branches";

const branch = (over: Partial<Branch> = {}): Branch => ({
  name: "alice-dev",
  status: "ready",
  status_reason: null,
  base: "orders",
  snapshot: "s1",
  runtime: "mysql@8.4.13",
  node: "node1",
  created_at: "2026-11-26T05:00:00Z",
  expires_at: "2026-11-29T05:00:00Z",
  reset_at: null,
  storage: { used: 12 * 1024 * 1024, referenced: 512 * 1024 * 1024 },
  connection: { host: "10.0.0.1", port: 40001, user: "alice" },
  proxy: null,
  operation: null,
  owner: "user:alice@example.com",
  owner_name: "alice@example.com",
  id: "alice/dev",
  access: "private",
  checkpoints: true,
  ...over,
});

describe("BranchesTable", () => {
  it("shows age since creation and storage used for each branch", () => {
    const now = Date.parse("2026-11-26T08:00:00Z");
    const html = renderToStaticMarkup(
      <BranchesTable
        branches={[branch(), branch({ name: "empty", storage: null, created_at: "" })]}
        now={now}
        onOpen={() => {}}
      />,
    );
    expect(html).toContain(">Created<");
    expect(html).toContain(">Storage<");
    expect(html).toContain("3h ago");
    expect(html).toContain("12 MiB");
    expect(html).toContain("—");
  });
});
