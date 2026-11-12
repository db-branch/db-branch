import { describe, expect, it } from "vitest";
import { createSeenStore, placePopover, resolveSteps, showsTour, tourKey, tourSteps } from "./tour";

const user = { user: { id: "u1", name: "Alice", roles: ["user"] }, csrf_token: "x", branch_prefix: "alice-" };

function memoryStorage() {
  const m = new Map<string, string>();
  return { getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => void m.set(k, v) };
}
const broken = () => {
  throw new Error("denied");
};

describe("showsTour", () => {
  it("サインイン済みの一般ユーザーに出す", () => {
    expect(showsTour(user)).toBe(true);
  });
  it("未ログインには出さない", () => {
    expect(showsTour(null)).toBe(false);
    expect(showsTour(undefined)).toBe(false);
  });
  it("管理者には出さない", () => {
    expect(showsTour({ ...user, user: { ...user.user, roles: ["admin"] } })).toBe(false);
    expect(showsTour({ ...user, user: { ...user.user, roles: ["scope:full"] } })).toBe(false);
  });
});

describe("createSeenStore", () => {
  it("ユーザーごとに見たことを覚える", () => {
    const store = memoryStorage();
    const seen = createSeenStore(() => store);
    expect(seen.has("u1")).toBe(false);
    seen.mark("u1");
    expect(seen.has("u1")).toBe(true);
    expect(seen.has("u2")).toBe(false);
    expect(store.getItem(tourKey("u1"))).toBe("1");
    expect(tourKey("u1")).toBe("db-branch.tour.v1.u1");
  });
  it("ページを開き直しても(同じストレージで)覚えている", () => {
    const store = memoryStorage();
    createSeenStore(() => store).mark("u1");
    expect(createSeenStore(() => store).has("u1")).toBe(true);
  });
  it("ストレージが使えなくても落ちず、そのページの間だけ覚える", () => {
    const seen = createSeenStore(broken);
    expect(seen.has("u1")).toBe(false);
    expect(() => seen.mark("u1")).not.toThrow();
    expect(seen.has("u1")).toBe(true);
    expect(seen.has("u2")).toBe(false);
  });
  it("書き込みだけ失敗しても落ちない", () => {
    const seen = createSeenStore(() => ({ getItem: () => null, setItem: broken }));
    seen.mark("u1");
    expect(seen.has("u1")).toBe(true);
  });
});

describe("tourSteps", () => {
  it("5 ステップで、それぞれ本物の要素を指す", () => {
    const steps = tourSteps("alice-");
    expect(steps.map((s) => s.id)).toEqual(["base", "name", "ttl", "create", "branches"]);
    for (const s of steps) expect(s.anchors.length).toBeGreaterThan(0);
  });
  it("名前のステップに固定の前置きを書く", () => {
    expect(tourSteps("alice-")[1].body).toContain("alice-");
  });
  it("タイトルは画面の見出しに合わせ、ボタン名は引用符で示す", () => {
    const steps = tourSteps("alice-");
    expect(steps[4].title).toBe("Your branches");
    expect(steps[3].body).toContain('"Create branch"');
    expect(steps[4].body).toContain('"Show password"');
  });
  it("前置きがなくても文が成り立つ", () => {
    expect(tourSteps(undefined)[1].body).not.toContain("undefined");
  });
});

describe("resolveSteps", () => {
  const steps = tourSteps("alice-");
  it("要素がないステップは飛ばす", () => {
    const present = new Set(["[data-tour=base]", "[data-tour=mine]"]);
    const out = resolveSteps(steps, (sel) => present.has(sel));
    expect(out.map((s) => s.id)).toEqual(["base", "branches"]);
  });
  it("最初の候補がなければ代わりの要素を使う", () => {
    const out = resolveSteps(steps, (sel) => sel === "[data-tour=mine]");
    expect(out).toHaveLength(1);
    expect(out[0].anchor).toBe("[data-tour=mine]");
  });
  it("何もなければ空", () => {
    expect(resolveSteps(steps, () => false)).toEqual([]);
  });
});

describe("placePopover", () => {
  const vp = { width: 800, height: 600 };
  const size = { width: 300, height: 150 };
  it("入る限り対象の下に置く", () => {
    const p = placePopover({ top: 100, left: 50, bottom: 140, right: 250 }, size, vp);
    expect(p.top).toBe(152);
    expect(p.left).toBe(50);
  });
  it("下に入らなければ上に置く", () => {
    const p = placePopover({ top: 450, left: 50, bottom: 560, right: 250 }, size, vp);
    expect(p.top).toBe(450 - 12 - 150);
  });
  it("上下とも入らなければ画面の下端に収める", () => {
    const p = placePopover({ top: 10, left: 50, bottom: 590, right: 250 }, size, vp);
    expect(p.top).toBe(600 - 150 - 8);
  });
  it("横は画面からはみ出さない(狭い画面)", () => {
    expect(placePopover({ top: 100, left: -40, bottom: 140, right: 10 }, size, vp).left).toBe(8);
  });
  it("スマホ幅では左右の余白を等しくして中央に置く", () => {
    const phone = { width: 390, height: 780 };
    for (const left of [16, 200, 350]) {
      const p = placePopover({ top: 100, left, bottom: 140, right: left + 20 }, { width: 340, height: 150 }, phone);
      expect(p.left).toBe(25);
      expect(phone.width - p.left - 340).toBe(25);
    }
  });
  it("対象がなければ中央", () => {
    const p = placePopover(null, size, vp);
    expect(p).toEqual({ top: 225, left: 250 });
  });
});
