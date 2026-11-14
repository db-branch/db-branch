import { describe, expect, it } from "vitest";
import { confirmReducer, countdown, elapsed, expiryState, extendTtl, hours, isBusy, longerChoices, sortBranches, ttlChoices } from "./branches";

const NOW = Date.parse("2026-01-01T12:00:00Z");
const at = (ms: number) => new Date(NOW + ms).toISOString();
const MIN = 60_000;
const HOUR = 60 * MIN;

describe("countdown", () => {
  it("時間と分で残りを言う", () => {
    expect(countdown(at(3 * HOUR + 12 * MIN), NOW)).toBe("in 3h 12m");
  });
  it("1 時間未満は分だけ", () => {
    expect(countdown(at(42 * MIN), NOW)).toBe("in 42m");
  });
  it("ちょうどの時間は分を省く", () => {
    expect(countdown(at(3 * HOUR), NOW)).toBe("in 3h");
  });
  it("1 日以上は日と時間", () => {
    expect(countdown(at(26 * HOUR), NOW)).toBe("in 1d 2h");
    expect(countdown(at(48 * HOUR), NOW)).toBe("in 2d");
  });
  it("1 分未満は <1m", () => {
    expect(countdown(at(30_000), NOW)).toBe("in <1m");
  });
  it("過ぎていれば expired、期限なしは no expiry", () => {
    expect(countdown(at(-1), NOW)).toBe("expired");
    expect(countdown(null, NOW)).toBe("no expiry");
  });
});

describe("expiryState", () => {
  it("1 時間以内は soon", () => {
    expect(expiryState(at(59 * MIN), NOW)).toBe("soon");
    expect(expiryState(at(HOUR), NOW)).toBe("soon");
  });
  it("1 時間より先は ok", () => {
    expect(expiryState(at(HOUR + 1), NOW)).toBe("ok");
  });
  it("過ぎていれば expired（ちょうど今も）", () => {
    expect(expiryState(at(0), NOW)).toBe("expired");
    expect(expiryState(at(-HOUR), NOW)).toBe("expired");
  });
  it("期限なしは none", () => {
    expect(expiryState(null, NOW)).toBe("none");
  });
});

describe("sortBranches", () => {
  const b = (name: string, ms: number | null) => ({ name, expires_at: ms === null ? null : at(ms) });
  it("期限が近い順、期限なしは最後", () => {
    const sorted = sortBranches([b("c", null), b("b", 5 * HOUR), b("a", HOUR), b("d", -HOUR)]);
    expect(sorted.map((x) => x.name)).toEqual(["d", "a", "b", "c"]);
  });
  it("同じ期限は名前順", () => {
    expect(sortBranches([b("y", HOUR), b("x", HOUR)]).map((x) => x.name)).toEqual(["x", "y"]);
  });
  it("指定した名前を先頭に置く", () => {
    expect(sortBranches([b("a", HOUR), b("z", 9 * HOUR)], ["z"]).map((x) => x.name)).toEqual(["z", "a"]);
  });
  it("元の配列を変えない", () => {
    const list = [b("b", 2 * HOUR), b("a", HOUR)];
    sortBranches(list);
    expect(list.map((x) => x.name)).toEqual(["b", "a"]);
  });
});

describe("extendTtl", () => {
  it("いまの残りに 4 時間を足した分数を返す（API は今からの期間を取る）", () => {
    expect(extendTtl(at(30 * MIN), NOW)).toBe("270m");
  });
  it("残りは切り上げる", () => {
    expect(extendTtl(at(30 * MIN + 1), NOW)).toBe("271m");
  });
  it("過ぎていれば刻みの分だけ", () => {
    expect(extendTtl(at(-HOUR), NOW)).toBe("240m");
  });
  it("期限なしでも刻みの分だけ", () => {
    expect(extendTtl(null, NOW)).toBe("240m");
  });
  it("刻みを選べる", () => {
    expect(extendTtl(at(HOUR), NOW, 60)).toBe("120m");
  });
});

describe("longerChoices", () => {
  it("残りより長い選択肢だけ残す（短くしてしまう延長を出さない）", () => {
    const names = longerChoices(ttlChoices, at(5 * HOUR), NOW).map(([v]) => v);
    expect(names).toEqual(["24h", "72h", "168h"]);
  });
  it("期限なしなら全部", () => {
    expect(longerChoices(ttlChoices, null, NOW)).toHaveLength(ttlChoices.length);
  });
});

describe("hours", () => {
  it("1h30m のような期間を時間にする", () => {
    expect(hours("4h")).toBe(4);
    expect(hours("1h30m")).toBe(1.5);
    expect(hours("")).toBe(0);
  });
});

describe("elapsed", () => {
  it("経過秒を短く書く", () => {
    expect(elapsed(NOW - 7_000, NOW)).toBe("7s");
    expect(elapsed(NOW - 65_000, NOW)).toBe("1m 05s");
  });
  it("未来の時刻は 0s", () => {
    expect(elapsed(NOW + 5000, NOW)).toBe("0s");
  });
});

describe("isBusy", () => {
  it("準備中の状態だけ true", () => {
    expect(isBusy("provisioning")).toBe(true);
    expect(isBusy("resetting")).toBe(true);
    expect(isBusy("ready")).toBe(false);
    expect(isBusy("failed")).toBe(false);
    expect(isBusy("lost")).toBe(false);
  });
});

describe("confirmReducer", () => {
  it("押すと確認待ちになる", () => {
    expect(confirmReducer(null, { type: "ask", what: "delete" })).toBe("delete");
  });
  it("別の操作を押すと確認待ちが入れ替わる", () => {
    expect(confirmReducer("delete", { type: "ask", what: "reset" })).toBe("reset");
  });
  it("取り消し・時間切れ・実行で元に戻る", () => {
    for (const type of ["cancel", "timeout", "done"] as const) expect(confirmReducer("delete", { type })).toBeNull();
  });
});

describe("isBusy: pause", () => {
  it("counts pausing and resuming as busy, and a paused branch as at rest", () => {
    expect(isBusy("pausing")).toBe(true);
    expect(isBusy("resuming")).toBe(true);
    expect(isBusy("paused")).toBe(false);
  });
});

