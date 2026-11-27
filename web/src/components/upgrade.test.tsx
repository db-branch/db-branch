import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Branch } from "../api";
import { Findings, headline, NotInstalled, UpgradeConfirm, UpgradePanel, UpgradeResultView } from "./upgrade";
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
  it("says how it ended in a few words, with the Checkpoint to restore", () => {
    expect(headline(result())).toBe("Upgraded to mysql@8.4.13 · before-upgrade-20261113-120000");
    expect(headline(result({ status: "checked" }))).toBe("Check passed");
    expect(headline(result({ status: "checked", pre_check: [{ level: "error", code: "c", message: "m" }] }))).toBe("Check found errors");
    expect(headline(result({ status: "rolled_back" }))).toBe("Rolled back");
    expect(headline(result({ status: "failed" }))).toBe("Failed");
    expect(headline(result({ status: "refused" }))).toBe("Refused");
    expect(headline(result({ status: "aborted" }))).toBe("Not started");
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

describe("NotInstalled", () => {
  it("lists the runtimes that could be installed, each with its command, when none is installed", () => {
    const h = renderToStaticMarkup(<NotInstalled runtime="mysql@5.7.44" targets={[]} toInstall={["mysql@8.0.46"]} />);
    expect(h).toContain("Not installed");
    expect(h).toContain("db-branch runtime install mysql@8.0.46");
    expect(h).not.toContain("None of the versions");
  });
  it("lists them behind an installed target as well", () => {
    const h = renderToStaticMarkup(<NotInstalled runtime="mysql@8.0.46" targets={["mysql@8.4.13"]} toInstall={["mysql@8.4.14", "mysql@8.4.15"]} />);
    expect(h).toContain("Not installed");
    expect(h).toContain("db-branch runtime install mysql@8.4.14");
    expect(h).toContain("db-branch runtime install mysql@8.4.15");
  });
  it("says there is nothing when there is neither, and says nothing when an installed target is enough", () => {
    expect(renderToStaticMarkup(<NotInstalled runtime="mysql@9.6.0" targets={[]} toInstall={[]} />)).toContain("No upgrade target");
    expect(renderToStaticMarkup(<NotInstalled runtime="mysql@8.0.46" targets={["mysql@8.4.13"]} toInstall={[]} />)).toBe("");
  });
});

describe("UpgradeConfirm", () => {
  it("shows checkpoint/rollback note and pre-check warnings", () => {
    const h = html(
      <UpgradeConfirm
        from="postgres@18.6"
        to="postgres@19.0"
        force={false}
        warnings={[{ level: "warning", code: "many_relations", message: "The cluster has 15000 relations." }]}
        checking={false}
        onBack={() => {}}
        onConfirm={() => {}}
      />,
    );
    expect(h).toContain("A checkpoint is saved first; on failure it rolls back automatically.");
    expect(h).toContain("15000 relations");
  });
});
