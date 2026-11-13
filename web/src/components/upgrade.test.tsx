import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Branch } from "../api";
import { Findings, headline, UpgradePanel, UpgradeResultView } from "./upgrade";
import type { UpgradeResult } from "../lib/upgrade";

const html = (node: React.ReactElement) => renderToStaticMarkup(node);
const result = (over: Partial<UpgradeResult> = {}): UpgradeResult => ({
  branch: "x", from: "mysql@8.0.46", to: "mysql@8.4.13", status: "upgraded", check_only: false, checkpoint: "before-upgrade-20261113-120000",
  pre_check: [{ level: "info", code: "upgrade_path", message: "a to b" }, { level: "warning", code: "native_password_users", message: "1 account(s) use mysql_native_password" }],
  log: [{ level: "warning", code: "log_warning", message: "MY-010068: CA certificate is self signed" }], ...over,
});
const branch = { name: "x", runtime: "mysql@8.0.46", base_runtime: "mysql@8.0.46", checkpoints: true } as Branch;

describe("the result of an upgrade", () => {
  it("lists the pre-check findings and the log warnings, errors first", () => {
    const h = html(<UpgradeResultView result={result({ pre_check: [{ level: "info", code: "a", message: "info line" }, { level: "error", code: "b", message: "error line" }] })} />);
    expect(h).toContain("Pre-check");
    expect(h).toContain("Server log during the upgrade");
    expect(h.indexOf("error line")).toBeLessThan(h.indexOf("info line"));
    expect(h).toContain("MY-010068");
    expect(h).toContain("upgrade-good");
  });
  it("says how it ended, with the Checkpoint to restore", () => {
    expect(headline(result())).toContain("before-upgrade-20261113-120000");
    expect(headline(result({ status: "checked" }))).toContain("can move to mysql@8.4.13");
    expect(headline(result({ status: "checked", pre_check: [{ level: "error", code: "c", message: "m" }] }))).toContain("errors below");
    expect(headline(result({ status: "rolled_back" }))).toContain("rolled back to mysql@8.0.46");
    expect(headline(result({ status: "failed" }))).toContain("left as it is");
    expect(headline(result({ status: "refused" }))).toContain("not changed");
    expect(headline(result({ status: "aborted" }))).toContain("did not start");
  });
  it("shows the end of the error log of a failed start, and the error", () => {
    const h = html(<UpgradeResultView result={result({ status: "rolled_back", error: "server stopped while starting", log_excerpt: "[ERROR] [MY-013170] Upgrade failed" })} />);
    expect(h).toContain("End of the server&#x27;s error log");
    expect(h).toContain("Upgrade failed");
    expect(h).toContain("server stopped while starting");
    expect(h).toContain("upgrade-bad");
  });
  it("shows nothing for an empty list", () => {
    expect(html(<Findings title="t" list={[]} />)).toBe("");
  });
});

describe("the Upgrade button", () => {
  const panel = (over: { b?: Partial<Branch>; ready?: boolean; allow?: (a: string) => boolean } = {}) =>
    html(<UpgradePanel branch={{ ...branch, ...over.b }} ready={over.ready ?? true} allow={over.allow ?? (() => true)} onChange={() => {}} />);
  it("is there and enabled for a ready Branch the person may upgrade", () => {
    const h = panel();
    expect(h).toContain("Upgrade");
    expect(h).not.toContain("disabled");
  });
  it("says why it is disabled", () => {
    expect(panel({ allow: () => false })).toContain("Your role cannot do this.");
    expect(panel({ ready: false })).toContain("Available once it is ready.");
    expect(panel({ b: { checkpoints: false } })).toContain("own ZFS");
  });
});
