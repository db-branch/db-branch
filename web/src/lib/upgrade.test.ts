import { describe, expect, it } from "vitest";
import { canForce, countLevel, installHint, isUpgraded, resultTone, stepReducer, upgradeChoices, upgradeProblem, upgradeTargets, type Step, type UpgradeResult } from "./upgrade";

const result = (over: Partial<UpgradeResult> = {}): UpgradeResult => ({ branch: "x", from: "mysql@8.0.46", to: "mysql@8.4.13", status: "upgraded", check_only: false, pre_check: [], log: [], ...over });

describe("which runtimes a Branch may move to (the server's rules)", () => {
  it("allows 8.0 to 8.4, 8.4 to 9.x and a patch update", () => {
    expect(upgradeProblem("mysql@8.0.46", "mysql@8.4.13")).toBe("");
    expect(upgradeProblem("mysql@8.4.13", "mysql@9.4.0")).toBe("");
    expect(upgradeProblem("mysql@8.4.9", "mysql@8.4.13")).toBe("");
    expect(upgradeProblem("postgres@17.2", "postgres@17.11")).toBe("");
    expect(upgradeProblem("postgres@19.0", "postgres@19.1")).toBe("");
  });
  it("refuses a skipped long-term-support series, a downgrade, the same version and another engine", () => {
    expect(upgradeProblem("mysql@8.0.46", "mysql@9.4.0")).toContain("8.4");
    expect(upgradeProblem("mysql@8.4.13", "mysql@9.8.0")).toContain("9.7");
    expect(upgradeProblem("mysql@8.4.13", "mysql@8.0.46")).toContain("Downgrades");
    expect(upgradeProblem("mysql@8.4.13", "mysql@8.4.13")).toContain("already");
    expect(upgradeProblem("mysql@8.4.13", "postgres@18.6")).toContain("one engine");
    expect(upgradeProblem("mysql@5.5.62", "mysql@5.7.44")).toContain("start from MySQL 5.6");
  });
  it("allows 5.6 to 5.7 and 5.7 to 8.0, and never skips a series (5.7 is one)", () => {
    expect(upgradeProblem("mysql@5.6.51", "mysql@5.7.44")).toBe("");
    expect(upgradeProblem("mysql@5.7.44", "mysql@8.0.46")).toBe("");
    expect(upgradeProblem("mysql@5.7.30", "mysql@5.7.44")).toBe("");
    expect(upgradeProblem("mysql@5.6.51", "mysql@8.0.46")).toContain("5.7");
    expect(upgradeProblem("mysql@5.7.44", "mysql@8.4.13")).toContain("8.0");
    expect(upgradeProblem("mysql@8.0.46", "mysql@5.7.44")).toContain("Downgrades");
  });
  it("allows PostgreSQL major upgrades and refuses downgrades", () => {
    expect(upgradeProblem("postgres@17.11", "postgres@18.6")).toBe("");
    expect(upgradeProblem("postgres@18.7", "postgres@19.0")).toBe("");
    expect(upgradeProblem("postgres@17.12", "postgres@19.0")).toBe("");
    expect(upgradeProblem("postgres@19.0", "postgres@18.7")).toContain("Downgrades");
    expect(upgradeProblem("postgres@9.6.1", "postgres@10.1")).toBe("");
  });
  it("lists the installed targets, oldest first, leaving out the ones it cannot reach", () => {
    expect(upgradeTargets("mysql@8.0.46", ["postgres@18.6", "mysql@8.0.46", "mysql@9.4.0", "mysql@8.4.13", "mysql@8.2.0"])).toEqual(["mysql@8.2.0", "mysql@8.4.13"]);
    expect(upgradeTargets("mysql@8.4.13", ["mysql@8.4.13"])).toEqual([]);
  });
  it("tells how to install a runtime and whether a Branch was upgraded", () => {
    expect(installHint("mysql@8.4.13")).toBe("db-branch runtime install mysql@8.4.13");
    expect(isUpgraded("mysql@8.4.13", "mysql@8.0.46")).toBe(true);
    expect(isUpgraded("mysql@8.4.13", "mysql@8.4.13")).toBe(false);
    expect(isUpgraded("mysql@8.4.13")).toBe(false);
  });
});

describe("how an upgrade result reads", () => {
  it("is good when it worked, warm for a check with warnings or a log with errors, bad otherwise", () => {
    expect(resultTone(result())).toBe("good");
    expect(resultTone(result({ log: [{ level: "error", code: "log_error", message: "x" }] }))).toBe("warn");
    expect(resultTone(result({ status: "checked", pre_check: [{ level: "warning", code: "c", message: "m" }] }))).toBe("warn");
    expect(resultTone(result({ status: "checked", pre_check: [{ level: "error", code: "c", message: "m" }] }))).toBe("bad");
    expect(resultTone(result({ status: "checked" }))).toBe("good");
    for (const status of ["rolled_back", "failed", "refused", "aborted"] as const) expect(resultTone(result({ status }))).toBe("bad");
  });
  it("offers to force only what the pre-check refused", () => {
    expect(canForce(result({ status: "refused" }))).toBe(true);
    expect(canForce(result({ status: "checked", pre_check: [{ level: "error", code: "c", message: "m" }] }))).toBe(true);
    expect(canForce(result({ status: "checked" }))).toBe(false);
    expect(canForce(result({ status: "rolled_back" }))).toBe(false);
    expect(countLevel([{ level: "error", code: "a", message: "" }, { level: "info", code: "b", message: "" }], "error")).toBe(1);
  });
});

describe("the steps of the dialog", () => {
  const closed: Step = { kind: "closed" };
  it("goes choose, confirm, run, done and back", () => {
    let s = stepReducer(closed, { type: "open" });
    expect(s).toEqual({ kind: "choose" });
    s = stepReducer(s, { type: "ask", target: "mysql@8.4.13" });
    expect(s).toEqual({ kind: "confirm", target: "mysql@8.4.13", force: false });
    expect(stepReducer(s, { type: "back" })).toEqual({ kind: "choose" });
    s = stepReducer(s, { type: "run", target: "mysql@8.4.13", checkOnly: false });
    expect(s).toEqual({ kind: "running", target: "mysql@8.4.13", checkOnly: false, phase: "" });
    s = stepReducer(s, { type: "phase", phase: "upgrading" });
    expect(s).toMatchObject({ phase: "upgrading" });
    expect(stepReducer(s, { type: "close" })).toBe(s); // a running upgrade cannot be closed
    s = stepReducer(s, { type: "finish", target: "mysql@8.4.13", result: result() });
    expect(s).toMatchObject({ kind: "done" });
    expect(stepReducer(s, { type: "ask", target: "mysql@8.4.13", force: true })).toMatchObject({ kind: "confirm", force: true });
    expect(stepReducer(s, { type: "open" })).toEqual({ kind: "choose" });
    expect(stepReducer(s, { type: "close" })).toEqual(closed);
  });
  it("ignores actions that do not fit the step", () => {
    expect(stepReducer(closed, { type: "ask", target: "x" })).toBe(closed);
    expect(stepReducer(closed, { type: "back" })).toBe(closed);
    expect(stepReducer(closed, { type: "run", target: "x", checkOnly: true })).toBe(closed);
    expect(stepReducer(closed, { type: "phase", phase: "p" })).toBe(closed);
    expect(stepReducer(closed, { type: "finish", target: "x" })).toBe(closed);
    const choose: Step = { kind: "choose" };
    expect(stepReducer(choose, { type: "open" })).toBe(choose);
    expect(stepReducer(choose, { type: "run", target: "x", checkOnly: true })).toMatchObject({ kind: "running", checkOnly: true });
  });
});

describe("upgradeChoices", () => {
  const available = ["mysql@9.6.0", "mysql@8.4.13", "mysql@8.0.46", "mysql@5.7.44", "mysql@5.6.51", "postgres@18.6"];
  it("separates the targets that are installed from the ones that could be installed, oldest first", () => {
    const c = upgradeChoices("mysql@5.7.44", ["mysql@5.7.44", "mysql@8.4.13", "postgres@18.6"], available);
    expect(c.targets).toEqual([]); // 8.4 is installed but cannot be reached from 5.7
    expect(c.toInstall).toEqual(["mysql@8.0.46"]);
  });
  it("offers an installed target and lists the next ones to install behind it", () => {
    const c = upgradeChoices("mysql@5.6.51", ["mysql@5.6.51", "mysql@5.7.44"], available);
    expect(c.targets).toEqual(["mysql@5.7.44"]);
    expect(c.toInstall).toEqual([]); // 8.0 would skip 5.7
  });
  it("lists a target once, even when it is both installed and in the catalog, and ignores other engines", () => {
    const c = upgradeChoices("mysql@8.0.46", ["mysql@8.0.46", "mysql@8.4.13"], available);
    expect(c.targets).toEqual(["mysql@8.4.13"]);
    expect(c.toInstall).toEqual([]);
    expect(upgradeChoices("mysql@9.6.0", ["mysql@9.6.0"], available)).toEqual({ targets: [], toInstall: [] });
  });
});
