import { describe, expect, it } from "vitest";
import { arrange, Choice, initialBase, matches, moveSelection, pushRecent } from "./bases";

const c = (name: string, runtime: string, description = ""): Choice => ({ name, runtime, description, snapshot: "s1", snapshotAt: null, public: false, masked: false });
const shop = c("shop", "mysql@8.4.13", "Orders and customers of the web shop");
const crm = c("crm", "postgres@16.4", "Sales CRM, masked");

describe("matches", () => {
  it("単語を順不同で名前・説明・エンジン・バージョンから探す", () => {
    expect(matches(shop, "customers orders", "all")).toBe(true);
    expect(matches(shop, "8.4 mysql", "all")).toBe(true);
    expect(matches(crm, "postgresql", "all")).toBe(true);
    expect(matches(crm, "pg 16", "all")).toBe(true);
    expect(matches(shop, "crm", "all")).toBe(false);
  });
  it("空の検索はすべて通し、エンジンで絞れる", () => {
    expect(matches(shop, "  ", "all")).toBe(true);
    expect(matches(shop, "", "postgres")).toBe(false);
    expect(matches(crm, "", "postgres")).toBe(true);
  });
});

describe("arrange", () => {
  it("最近使った順を先に、残りは名前順", () => {
    const list = [shop, crm, c("analytics", "mysql@8.4")];
    expect(arrange(list, ["shop", "crm"]).map((x) => x.name)).toEqual(["shop", "crm", "analytics"]);
    expect(arrange(list, []).map((x) => x.name)).toEqual(["analytics", "crm", "shop"]);
  });
});

describe("pushRecent", () => {
  it("先頭に置き、重複を除き、上限で切る", () => {
    expect(pushRecent(["a", "b"], "b")).toEqual(["b", "a"]);
    expect(pushRecent(["1", "2", "3", "4", "5", "6", "7", "8"], "9")).toHaveLength(8);
  });
});

describe("initialBase", () => {
  it("最後に使ったベースが残っていればそれ、なければ先頭", () => {
    expect(initialBase([crm, shop], ["shop"])).toBe("shop");
    expect(initialBase([crm, shop], ["gone"])).toBe("crm");
  });
});

describe("moveSelection", () => {
  it("矢印キーで端を越えずに動く", () => {
    expect(moveSelection(["a", "b", "c"], "a", 1)).toBe("b");
    expect(moveSelection(["a", "b", "c"], "c", 1)).toBe("c");
    expect(moveSelection(["a", "b", "c"], "a", -1)).toBe("a");
    expect(moveSelection(["a", "b"], "x", 1)).toBe("b");
    expect(moveSelection([], "x", 1)).toBe("x");
  });
});
