import type { ReactElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import type { Branch } from "../api";
import { createResetHandlers, ResetBranch } from "./BranchDetail";

const sampleBranch = {
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
} as Branch;

function findButtons(node: unknown): Array<{ props: { className?: string; onClick?: () => void } }> {
  if (!node || typeof node !== "object") return [];
  const el = node as ReactElement<{ className?: string; onClick?: () => void; children?: unknown }>;
  const out: Array<{ props: { className?: string; onClick?: () => void } }> = [];
  if (el.type === "button") out.push({ props: el.props });
  const kids = el.props?.children;
  if (Array.isArray(kids)) {
    for (const k of kids) out.push(...findButtons(k));
  } else if (kids) {
    out.push(...findButtons(kids));
  }
  return out;
}

describe("ResetBranch", () => {
  it("renders a confirmation dialog and wires Cancel and Confirm reset callbacks", () => {
    const onClose = vi.fn();
    const onConfirm = vi.fn();
    const tree = ResetBranch({ branch: sampleBranch, onClose, onConfirm });
    const h = renderToStaticMarkup(tree);
    expect(h).toContain("Reset alice-feature-x?");
    expect(h).toContain("Changes you made are lost. Checkpoints are kept.");
    expect(h).toContain("Cancel");
    expect(h).toContain("Confirm reset");

    const buttons = findButtons(tree);
    expect(buttons).toHaveLength(2);
    buttons[0].props.onClick?.();
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(onConfirm).not.toHaveBeenCalled();

    buttons[1].props.onClick?.();
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it("opens the confirmation dialog instead of resetting directly, and invokes reset only on confirm", async () => {
    const resetApi = vi.fn().mockResolvedValue(undefined);
    const setResetting = vi.fn();
    const run = vi.fn((p: Promise<unknown>) => p);
    const handlers = createResetHandlers("alice-feature-x", setResetting, run, resetApi);

    handlers.onReset();
    expect(setResetting).toHaveBeenCalledWith(true);
    expect(resetApi).not.toHaveBeenCalled();

    handlers.onClose();
    expect(setResetting).toHaveBeenLastCalledWith(false);
    expect(resetApi).not.toHaveBeenCalled();

    await handlers.onConfirm();
    expect(setResetting).toHaveBeenLastCalledWith(false);
    expect(resetApi).toHaveBeenCalledWith("alice-feature-x");
    expect(run).toHaveBeenCalledTimes(1);
  });
});
