import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it } from "vitest";
import type { Base, Branch, Checkpoint, Line } from "../api";
import { PanelBody, type Panel } from "../components/checkpoints";
import { UpgradePanel } from "../components/upgrade";
import { DeleteBase, UpdateData } from "../pages/Bases";
import { DeleteBranch } from "../pages/Branches";
import { relative } from "./format";
import { setLanguage } from "./i18n";
import { layoutGraph } from "./historygraph";

afterEach(() => setLanguage("en"));

const noop = () => {};

const branch: Branch = {
  name: "dev-test",
  status: "ready",
  status_reason: null,
  base: "shop",
  snapshot: "s1",
  runtime: "mysql@8.4.13",
  node: "n1",
  created_at: new Date(Date.now() - 120_000).toISOString(),
  expires_at: new Date(Date.now() + 3600_000).toISOString(),
  reset_at: null,
  storage: { used: 1024, referenced: 2048 },
  connection: { host: "127.0.0.1", port: 40001, user: "dev" },
  proxy: null,
  operation: null,
  owner: "dev",
  id: "dev/test",
  access: "private",
  checkpoints: true,
};

const base: Base = {
  name: "shop",
  runtime: "mysql@8.4.13",
  status: "ready",
  status_reason: null,
  node: "n1",
  port: 3306,
  created_at: new Date(Date.now() - 120_000).toISOString(),
  latest_snapshot: "s1",
  snapshots: [{ name: "s1", created_at: new Date().toISOString(), public: false }],
  replicas: {},
  operation: null,
  description: "",
};

describe("Japanese UI rendering", () => {
  it("renders DeleteBranch confirmation dialog in Japanese", () => {
    setLanguage("ja");
    const html = renderToStaticMarkup(<DeleteBranch branch={branch} onClose={noop} />);
    expect(html).toContain("dev-test を削除しますか？");
    expect(html).toContain("この Branch のデータは失われます");
    expect(html).not.toContain("Data in this branch is lost");
  });

  it("renders DeleteBase and UpdateData in Japanese", () => {
    setLanguage("ja");
    const delEmpty = renderToStaticMarkup(<DeleteBase base={base} branches={0} onClose={noop} onDeleted={noop} />);
    expect(delEmpty).toContain("Base shop を削除しますか？");
    expect(delEmpty).toContain("本当によろしいですか？");
    expect(delEmpty).not.toContain("Are you sure?");

    const delUsed = renderToStaticMarkup(<DeleteBase base={base} branches={2} onClose={noop} onDeleted={noop} />);
    expect(delUsed).toContain("2 個の Branch がまだこの Base を使っています。");
    expect(delUsed).not.toContain("still use this base");

    const upd = renderToStaticMarkup(<UpdateData base={base} onClose={noop} />);
    expect(upd).toContain("shop を更新");
    expect(upd).toContain("あとで Snapshot を取る（新しい Branch が使います）");
    expect(upd).not.toContain("Take a snapshot afterwards");
  });

  it("renders CheckpointPanel restore and delete confirmations in Japanese", () => {
    setLanguage("ja");
    const cp: Checkpoint = { name: "cp1", created_at: new Date().toISOString(), size: 1, line: 0, auto: false, current: true };
    const lines: Line[] = [{ line: 0, live: true, parent: null, from: "" }];
    const g = layoutGraph({ lines, checkpoints: [cp], head: "cp1" });
    const panel: Panel = {
      branch,
      graph: { items: [cp], lines, live: 0, head: "cp1" },
      rows: [cp],
      events: undefined,
      fromTx: undefined,
      error: null,
      note: "",
      warning: "",
      working: false,
      name: "",
      setName: noop,
      problem: null,
      saveWhy: undefined,
      restoreWhy: undefined,
      deleteWhy: undefined,
      forkWhy: undefined,
      mergeWhy: undefined,
      txRestoreWhy: undefined,
      save: noop,
      saveFirst: noop,
      selected: g.nodes.find((n) => n.kind === "checkpoint")!,
      select: noop,
      asking: { what: "restore", name: "cp1", from: "graph" },
      setAsking: noop,
      askingTx: false,
      setAskingTx: noop,
      restoreCheckpoint: noop,
      deleteCheckpoint: noop,
      restoreTx: noop,
      shown: (l) => l,
      showLines: false,
    };
    const restoreHtml = renderToStaticMarkup(<PanelBody p={panel} onExpand={noop} />);
    expect(restoreHtml).toContain("dev-test を cp1 の状態に戻しますか？");
    expect(restoreHtml).toContain("先に Checkpoint を保存");
    expect(restoreHtml).toContain("復元を実行");
    expect(restoreHtml).not.toContain("Save a checkpoint first");
    expect(restoreHtml).not.toContain("Confirm restore");

    const delHtml = renderToStaticMarkup(<PanelBody p={{ ...panel, asking: { what: "delete", name: "cp1", from: "list" } }} onExpand={noop} />);
    expect(delHtml).toContain("チェックポイント cp1 を削除しますか？");
    expect(delHtml).toContain("削除を実行");
    expect(delHtml).not.toContain("Confirm delete");
  });

  it("renders UpgradePanel disabled reason and relative time in Japanese", () => {
    setLanguage("ja");
    const upHtml = renderToStaticMarkup(<UpgradePanel branch={{ ...branch, checkpoints: false }} ready={true} allow={() => true} onChange={noop} />);
    expect(upHtml).toContain("アップグレードは Checkpoint と同じく、ノード自身の ZFS が必要です。");
    expect(relative(new Date(Date.now() - 120_000).toISOString())).toBe("2m前");
  });
});
