import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it } from "vitest";
import type { UpdateView } from "../api";
import { setLanguage } from "../lib/i18n";
import { UpdateConfirm, UpdatesPanel } from "./Settings";

const noop = () => {};

afterEach(() => {
  setLanguage("en");
});

const baseView: UpdateView = {
  checked_at: "2026-11-28T10:00:00Z",
  checked_by: "node1",
  nodes: [{ name: "node1", leader: true, down: false, version: "0.4.1", modules: {} }],
  packages: [
    {
      name: "db-branch",
      installed: "0.4.1",
      available: [
        { version: "2.0.0", kind: "major" },
        { version: "1.0.0", kind: "major0" },
        { version: "0.4.2", kind: "minor" },
      ],
    },
  ],
  latest_minor: "1.0.0",
  rollout: null,
};

describe("UpdatesPanel", () => {
  it("shows major0 candidate label and Update button when 1.0.0 is offered for 0.x", () => {
    const html = renderToStaticMarkup(
      <UpdatesPanel
        v={baseView}
        error={null}
        busy={false}
        confirming={null}
        cancelling={false}
        onCheck={noop}
        onConfirmVersion={noop}
        onCancelConfirm={noop}
        onStartUpdate={noop}
        onCancelRollout={noop}
      />,
    );
    expect(html).toContain("2.0.0");
    expect(html).toContain("major update — see docs");
    expect(html).toContain("1.0.0");
    expect(html).toContain("major update — 0.x → 1.x");
    expect(html).toContain("Update to 1.0.0");
    expect(html).not.toContain("A major update is done by hand");
  });

  it("shows manual-update hint when only 2.0.0 major update exists", () => {
    const html = renderToStaticMarkup(
      <UpdatesPanel
        v={{
          ...baseView,
          latest_minor: "",
          packages: [{ name: "db-branch", installed: "1.2.0", available: [{ version: "2.0.0", kind: "major" }] }],
        }}
        error={null}
        busy={false}
        confirming={null}
        cancelling={false}
        onCheck={noop}
        onConfirmVersion={noop}
        onCancelConfirm={noop}
        onStartUpdate={noop}
        onCancelRollout={noop}
      />,
    );
    expect(html).toContain("A major update is done by hand: see docs/updates.md.");
  });

  it("wires confirming major0 warning inside UpdatesPanel when confirming 1.0.0", () => {
    const html = renderToStaticMarkup(
      <UpdatesPanel
        v={baseView}
        error={null}
        busy={false}
        confirming="1.0.0"
        cancelling={false}
        onCheck={noop}
        onConfirmVersion={noop}
        onCancelConfirm={noop}
        onStartUpdate={noop}
        onCancelRollout={noop}
      />,
    );
    expect(html).toContain("Major update (0.x → 1.0). Read the upgrade notes in the release notes before starting.");
    expect(html).toContain("Update every node to 1.0.0?");
  });
});

describe("UpdateConfirm", () => {
  it("shows the 0.x -> 1.0 release notes warning only for major0 updates (en & ja)", () => {
    const enMajor0 = renderToStaticMarkup(<UpdateConfirm version="1.0.0" major0={true} busy={false} onCancel={noop} onConfirm={noop} />);
    expect(enMajor0).toContain("Major update (0.x → 1.0). Read the upgrade notes in the release notes before starting.");

    const enMinor = renderToStaticMarkup(<UpdateConfirm version="0.4.2" major0={false} busy={false} onCancel={noop} onConfirm={noop} />);
    expect(enMinor).not.toContain("Major update (0.x → 1.0)");

    setLanguage("ja");
    const jaMajor0 = renderToStaticMarkup(<UpdateConfirm version="1.0.0" major0={true} busy={false} onCancel={noop} onConfirm={noop} />);
    expect(jaMajor0).toContain("メジャー更新（0.x → 1.0）です。リリースノートの更新上の注意を読んでから開始してください。");
    expect(jaMajor0).not.toContain("Major update (0.x → 1.0)");
  });
});
