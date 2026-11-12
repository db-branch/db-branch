import { describe, expect, it } from "vitest";
import { groupSettings } from "./scopegroups";

const s = (key: string) => ({ key }) as never;

describe("groupSettings", () => {
  it("先頭の語が 2 種類以上なら、その語ごとの節に分ける（出現順）", () => {
    const g = groupSettings([s("backup.interval"), s("branch.ttl"), s("backup.keep"), s("zfs.pool"), s("custom.x")]);
    expect(g.map((x) => [x.prefix, x.label, x.items.map((i) => i.key)])).toEqual([
      ["backup", "Backups", ["backup.interval", "backup.keep"]],
      ["branch", "Branches", ["branch.ttl"]],
      ["zfs", "ZFS", ["zfs.pool"]],
      ["custom", "Custom", ["custom.x"]],
    ]);
  });
  it("ドットのない項目は見出しなしで先頭に置く", () => {
    const g = groupSettings([s("a.x"), s("plain"), s("b.y")]);
    expect(g.map((x) => x.prefix)).toEqual(["", "a", "b"]);
    expect(g[0].label).toBe("");
  });
  it("先頭の語が 1 種類だけなら分けない", () => {
    expect(groupSettings([s("backup.a"), s("backup.b")])).toHaveLength(1);
    expect(groupSettings([s("backup.a"), s("backup.b")])[0].label).toBe("");
    expect(groupSettings([s("x"), s("y")])).toHaveLength(1);
    expect(groupSettings([])).toEqual([]);
  });
  it("分けても全項目は 1 つずつ残る", () => {
    const keys = ["backup.a", "branch.b", "notify.c", "update.d", "console.e", "ports.f", "upload.g", "base.h"];
    expect(groupSettings(keys.map(s)).flatMap((g) => g.items.map((i) => i.key))).toEqual(keys);
    expect(groupSettings(keys.map(s)).map((g) => g.label)).toEqual(["Backups", "Branches", "Notifications", "Updates", "Console", "Ports", "Uploads", "Bases"]);
  });
});
