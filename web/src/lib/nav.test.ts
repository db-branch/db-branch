import { describe, expect, it } from "vitest";
import { buildNav, ancestorsOf, expandFor, legacyRedirect, loadOpen, resolveRoute, saveOpen, NAV_KEY } from "./nav";

const pages = [
  { module: "auth", path: "/users", title: "Users" },
  { module: "audit", path: "/log", title: "Audit log" },
];
const admin = { pages: ["branches", "bases", "cluster", "settings"], canSettings: true, enabled: ["auth", "audit", "masking"], modPages: pages };
const maintainer = { pages: ["branches", "bases", "cluster"], canSettings: false, enabled: undefined, modPages: [pages[0]] };

function memoryStorage() {
  const m = new Map<string, string>();
  return { getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => void m.set(k, v) };
}
const broken = () => {
  throw new Error("denied");
};

describe("buildNav", () => {
  it("管理者には Settings の下に Modules を出し、有効なモジュールごとに設定とページを並べる", () => {
    const nav = buildNav(admin);
    expect(nav.map((n) => n.id)).toEqual(["page:branches", "page:bases", "page:cluster", "page:settings", "modules"]);
    const modules = nav[4];
    expect(modules.children?.map((n) => n.id)).toEqual(["overview", "mod:audit", "mod:auth", "mod:masking"]);
    const auth = modules.children![2];
    expect(auth.label).toBe("Auth");
    expect(auth.children?.map((n) => [n.label, n.href])).toEqual([
      ["Settings", "/admin/modules/auth"],
      ["Users", "/admin/m/auth/users"],
    ]);
    expect(modules.children![0].href).toBe("/admin/modules");
  });
  it("モジュールのページは最上位に並べない", () => {
    expect(buildNav(admin).some((n) => n.id.startsWith("modpage:"))).toBe(false);
  });
  it("設定を見られない保守担当には、見られるモジュールのページだけを出す", () => {
    const modules = buildNav(maintainer).find((n) => n.id === "modules")!;
    expect(modules.children?.map((n) => n.id)).toEqual(["mod:auth"]);
    expect(modules.children![0].children?.map((n) => n.label)).toEqual(["Users"]);
  });
  it("見せるものがなければ Modules を出さない", () => {
    expect(buildNav({ ...maintainer, modPages: [] }).some((n) => n.id === "modules")).toBe(false);
  });
});

describe("resolveRoute", () => {
  it("ページ・概要・モジュール設定・モジュールページを見分ける", () => {
    expect(resolveRoute("/admin/cluster", admin)).toEqual({ kind: "page", page: "cluster" });
    expect(resolveRoute("/admin/settings/tokens", admin)).toEqual({ kind: "page", page: "settings" });
    expect(resolveRoute("/admin/modules", admin)).toEqual({ kind: "overview" });
    expect(resolveRoute("/admin/modules/auth", admin)).toEqual({ kind: "module", name: "auth" });
    expect(resolveRoute("/admin/m/auth/users", admin)).toEqual({ kind: "modpage", module: "auth", path: "/users" });
  });
  it("有効でないモジュールの設定は概要に戻す", () => {
    expect(resolveRoute("/admin/modules/ghost", admin)).toEqual({ kind: "overview" });
  });
  it("読み込み前は待つ", () => {
    expect(resolveRoute("/admin/m/auth/users", { ...admin, modPages: undefined })).toEqual({ kind: "pending" });
    expect(resolveRoute("/admin/modules/auth", { ...admin, enabled: undefined })).toEqual({ kind: "module", name: "auth" });
  });
  it("見られない場所や不明な場所は Branches", () => {
    expect(resolveRoute("/admin/modules", maintainer)).toEqual({ kind: "page", page: "branches" });
    expect(resolveRoute("/admin/settings", maintainer)).toEqual({ kind: "page", page: "branches" });
    expect(resolveRoute("/admin/m/audit/log", maintainer)).toEqual({ kind: "page", page: "branches" });
    expect(resolveRoute("/admin", admin)).toEqual({ kind: "page", page: "branches" });
  });
});

describe("ancestorsOf", () => {
  it("今のページを含む開閉を返す", () => {
    expect(ancestorsOf({ kind: "page", page: "bases" })).toEqual([]);
    expect(ancestorsOf({ kind: "overview" })).toEqual(["modules"]);
    expect(ancestorsOf({ kind: "module", name: "auth" })).toEqual(["modules", "mod:auth"]);
    expect(ancestorsOf({ kind: "modpage", module: "auth", path: "/users" })).toEqual(["modules", "mod:auth"]);
  });
});

describe("legacyRedirect", () => {
  it("設定のモジュール関連の古い URL を新しい場所へ", () => {
    expect(legacyRedirect("/admin/settings/modules")).toBe("/admin/modules");
    expect(legacyRedirect("/admin/settings/auth")).toBe("/admin/modules/auth");
    expect(legacyRedirect("/admin/settings/my%20mod")).toBe("/admin/modules/my%20mod");
  });
  it("設定の残るタブと他のページはそのまま", () => {
    for (const p of ["/admin/settings", "/admin/settings/", "/admin/settings/tokens", "/admin/settings/updates", "/admin/settings/general", "/admin/modules", "/admin/branches"]) expect(legacyRedirect(p)).toBeNull();
  });
});

describe("開閉の記憶", () => {
  it("保存した選択を読み戻す", () => {
    const s = memoryStorage();
    saveOpen({ modules: true, "mod:auth": false }, s);
    expect(s.getItem(NAV_KEY)).not.toBeNull();
    expect(loadOpen(s)).toEqual({ modules: true, "mod:auth": false });
  });
  it("壊れた値や使えない保存先では空で動く", () => {
    const s = memoryStorage();
    s.setItem(NAV_KEY, "{oops");
    expect(loadOpen(s)).toEqual({});
    s.setItem(NAV_KEY, JSON.stringify({ a: true, b: "x", c: 1 }));
    expect(loadOpen(s)).toEqual({ a: true });
    expect(loadOpen({ getItem: broken, setItem: broken })).toEqual({});
    expect(() => saveOpen({ a: true }, { getItem: broken, setItem: broken })).not.toThrow();
    expect(loadOpen(undefined)).toEqual({});
  });
  it("今のページの祖先は開き、ほかの選択は変えない", () => {
    expect(expandFor({ "mod:audit": true, modules: false }, ["modules", "mod:auth"])).toEqual({ "mod:audit": true, modules: true, "mod:auth": true });
    const same = { modules: true };
    expect(expandFor(same, ["modules"])).toBe(same);
  });
});
