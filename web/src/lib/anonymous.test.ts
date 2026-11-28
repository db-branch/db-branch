import { describe, expect, it } from "vitest";
import { anonymousCount, changes, inputsOf, parseMax, parseTTL, shortDuration } from "./anonymous";

describe("shortDuration", () => {
  it("サーバーの Go 形式の期間を入力しやすい形にする", () => {
    expect(shortDuration("4h0m0s")).toBe("4h");
    expect(shortDuration("30m0s")).toBe("30m");
    expect(shortDuration("1h30m0s")).toBe("1h30m");
    expect(shortDuration("45s")).toBe("45s");
    expect(shortDuration("24h0m0s")).toBe("24h");
  });
});

describe("parseMax", () => {
  it("空欄と inherit は継承、0 以上の整数はその数", () => {
    expect(parseMax("")).toEqual({ ok: true, value: null });
    expect(parseMax("  Inherit ")).toEqual({ ok: true, value: null });
    expect(parseMax("0")).toEqual({ ok: true, value: 0 });
    expect(parseMax(" 12 ")).toEqual({ ok: true, value: 12 });
  });
  it("負数・小数・文字は受け付けない", () => {
    for (const bad of ["-1", "1.5", "many", "1e3", "０"]) expect(parseMax(bad).ok).toBe(false);
  });
});

describe("parseTTL", () => {
  it("空欄と inherit は継承、正の期間はそのまま", () => {
    expect(parseTTL("")).toEqual({ ok: true, value: null });
    expect(parseTTL("inherit")).toEqual({ ok: true, value: null });
    expect(parseTTL("4h")).toEqual({ ok: true, value: "4h" });
    expect(parseTTL(" 1h30m ")).toEqual({ ok: true, value: "1h30m" });
  });
  it("0・負・単位なし・文字は受け付けない", () => {
    for (const bad of ["0", "0s", "0h0m", "0.0h", "-1h", "4", "soon", "4 hours", "1d"]) expect(parseTTL(bad).ok).toBe(false);
  });
});

describe("inputsOf", () => {
  it("設定を入力欄の文字にし、継承は空欄", () => {
    expect(inputsOf({ max_branches: null, max_ttl: null })).toEqual({ max: "", ttl: "" });
    expect(inputsOf({ max_branches: 0, max_ttl: "4h0m0s" })).toEqual({ max: "0", ttl: "4h" });
  });
});

describe("changes", () => {
  const cur = { max_branches: 3, max_ttl: "4h0m0s" };
  it("変えた項目だけを送り、継承は null で送る", () => {
    expect(changes(cur, "5", "4h")).toEqual({ ok: true, body: { max_branches: 5 } });
    expect(changes(cur, "", "4h")).toEqual({ ok: true, body: { max_branches: null } });
    expect(changes(cur, "3", "")).toEqual({ ok: true, body: { max_ttl: null } });
    expect(changes(cur, "0", "90m")).toEqual({ ok: true, body: { max_branches: 0, max_ttl: "90m" } });
  });
  it("何も変わらなければ body は空", () => {
    expect(changes(cur, "3", "4h")).toEqual({ ok: true, body: {} });
    expect(changes({ max_branches: null, max_ttl: null }, "", "inherit")).toEqual({ ok: true, body: {} });
  });
  it("不正な欄を名指しする", () => {
    expect(changes(cur, "x", "4h")).toEqual({ ok: false, field: "max" });
    expect(changes(cur, "3", "x")).toEqual({ ok: false, field: "ttl" });
    expect(changes(cur, "x", "x")).toEqual({ ok: false, field: "max" });
  });
});

describe("anonymousCount", () => {
  it("そのベースから作られたログインなしのブランチだけ数える", () => {
    const list = [
      { owner: "anonymous", base: "prod" },
      { owner: "anonymous", base: "prod" },
      { owner: "anonymous", base: "demo" },
      { owner: "alice", base: "prod" },
      { owner: null, base: "prod" },
    ];
    expect(anonymousCount(list, "prod")).toBe(2);
    expect(anonymousCount(list, "none")).toBe(0);
    expect(anonymousCount(undefined, "prod")).toBe(0);
  });
});
