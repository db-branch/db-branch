import { afterEach, describe, expect, it } from "vitest";
import { bytes, formatDate, formatDateTime, formatTime, relative, ttlState, statusShape, engineLabel, storageShare } from "./format";
import { setLanguage } from "./i18n";

afterEach(() => setLanguage("en"));

describe("locale date formatting", () => {
  it("formats dates and times in ja-JP when language is ja", () => {
    setLanguage("ja");
    const iso = "2026-11-24T10:15:30Z";
    expect(formatDate(iso)).toMatch(/^2026\/11\/2[45]$/);
    expect(formatDateTime(iso)).toMatch(/^2026\/11\/2[45] \d{2}:\d{2}:\d{2}$/);
    expect(formatTime(iso)).toMatch(/^\d{2}:\d{2}:\d{2}$/);
  });

  it("does not throw on invalid or empty timestamps", () => {
    for (const lang of ["ja", "en"] as const) {
      setLanguage(lang);
      expect(formatDate("")).toBe("—");
      expect(formatDateTime("")).toBe("—");
      expect(formatTime("")).toBe("—");
      expect(formatDate("not-a-date")).toBe("not-a-date");
      expect(formatDateTime("not-a-date")).toBe("not-a-date");
      expect(formatTime("not-a-date")).toBe("not-a-date");
    }
  });
});

const now = Date.parse("2026-11-09T12:00:00Z");

describe("bytes", () => {
  it("人間の単位で短く表す", () => {
    expect(bytes(0)).toBe("0 B");
    expect(bytes(512)).toBe("512 B");
    expect(bytes(1536)).toBe("1.5 KiB");
    expect(bytes(10 * 1024 * 1024)).toBe("10 MiB");
    expect(bytes(5 * 1024 ** 3)).toBe("5.0 GiB");
  });
});

describe("relative", () => {
  it("近い順に分・時間・日で表す", () => {
    expect(relative("2026-11-09T11:59:30Z", now)).toBe("now");
    expect(relative("2026-11-09T11:45:00Z", now)).toBe("15m ago");
    expect(relative("2026-11-09T09:00:00Z", now)).toBe("3h ago");
    expect(relative("2026-11-06T12:00:00Z", now)).toBe("3d ago");
    expect(relative("2026-11-09T14:00:00Z", now)).toBe("in 2h");
  });
});

describe("ttlState", () => {
  it("期限がなければ無期限", () => {
    expect(ttlState("2026-11-09T00:00:00Z", null, now)).toEqual({ kind: "none" });
  });
  it("残りの割合と残り時間を返し少なければ警告にする", () => {
    const s = ttlState("2026-11-09T00:00:00Z", "2026-11-10T00:00:00Z", now);
    expect(s).toEqual({ kind: "active", fraction: 0.5, left: "12h", warn: false });
    const low = ttlState("2026-11-09T00:00:00Z", "2026-11-09T13:00:00Z", now);
    expect(low.kind === "active" && low.warn).toBe(true);
  });
  it("過ぎていれば期限切れ", () => {
    expect(ttlState("2026-11-08T00:00:00Z", "2026-11-09T11:00:00Z", now)).toEqual({ kind: "expired" });
  });
});

describe("statusShape", () => {
  it("状態を形に対応させる", () => {
    expect(statusShape("ready")).toBe("dot");
    expect(statusShape("provisioning")).toBe("ring");
    expect(statusShape("resetting")).toBe("ring");
    expect(statusShape("deleting")).toBe("ring");
    expect(statusShape("maintenance")).toBe("ring");
    expect(statusShape("failed")).toBe("triangle");
    expect(statusShape("lost")).toBe("dashed");
  });
});

describe("engineLabel", () => {
  it("エンジンとバージョンに分ける", () => {
    expect(engineLabel("mysql@8.4.13")).toEqual({ engine: "mysql", short: "My", version: "8.4.13" });
    expect(engineLabel("postgres@17.2")).toEqual({ engine: "postgres", short: "Pg", version: "17.2" });
  });
});

describe("storageShare", () => {
  it("共有分と固有分の割合を返す", () => {
    expect(storageShare({ used: 25, referenced: 100 }, 200)).toEqual({ total: 0.5, own: 0.125 });
    expect(storageShare(null, 200)).toEqual({ total: 0, own: 0 });
    expect(storageShare({ used: 10, referenced: 0 }, 0)).toEqual({ total: 0, own: 0 });
  });
});

describe("statusShape: paused", () => {
  it("draws a paused branch as a pause mark, and pausing and resuming as busy", () => {
    expect(statusShape("paused")).toBe("pause");
    expect(statusShape("pausing")).toBe("ring");
    expect(statusShape("resuming")).toBe("ring");
  });
});

