import { describe, expect, it } from "vitest";
import { fullName, MAX_NAME, maxTyped, nameProblem, ownerPrefix, splitOwned, suggestName } from "./branchname";

describe("fullName", () => {
  it("入力した名前の前に前置きを付ける", () => {
    expect(fullName("alice-", "test")).toBe("alice-test");
  });
  it("すでに前置きが付いていれば重ねない", () => {
    expect(fullName("alice-", "alice-test")).toBe("alice-test");
  });
  it("前置きがなければ(管理者)そのまま", () => {
    expect(fullName("", "test")).toBe("test");
    expect(fullName(undefined, "test")).toBe("test");
  });
  it("何も入力していなければ空", () => {
    expect(fullName("alice-", "")).toBe("");
  });
});

describe("maxTyped", () => {
  it("名前全体の上限32文字から前置きを引く", () => {
    expect(maxTyped("alice-")).toBe(26);
    expect(maxTyped("")).toBe(32);
    expect(maxTyped(undefined)).toBe(32);
  });
  it("すでに前置きが付いた入力はその分を数えない", () => {
    expect(maxTyped("alice-", "alice-test")).toBe(32);
  });
});

describe("ownerPrefix", () => {
  it("メールアドレスはローカル部を小文字にし、記号を1つのハイフンにする", () => {
    expect(ownerPrefix("Demo.User@example.com")).toBe("demo-user");
  });
  it("空か数字で始まれば u を付ける", () => {
    expect(ownerPrefix("42")).toBe("u42");
    expect(ownerPrefix("")).toBe("u");
  });
});

describe("splitOwned", () => {
  it("所有者の前置きで始まる名前は前置きと残りに分ける", () => {
    expect(splitOwned("demo-test", "demo")).toEqual(["demo-", "test"]);
  });
  it("前置きで始まらない名前や前置きだけの名前は分けない", () => {
    expect(splitOwned("shop", "demo")).toEqual(["", "shop"]);
    expect(splitOwned("demo-", "demo")).toEqual(["", "demo-"]);
    expect(splitOwned("demo-test", undefined)).toEqual(["", "demo-test"]);
  });
});

describe("suggestName", () => {
  const rand = (...xs: number[]) => {
    let i = 0;
    return () => xs[i++ % xs.length];
  };
  it("ベース名に 3 文字の英数字を足す", () => {
    expect(suggestName("shop", "demo-", rand(0, 0.5, 0.99))).toBe("shop-as9");
  });
  it("入力欄の名前の規則を満たす", () => {
    const re = /^[a-z](?:[a-z0-9]|-[a-z0-9])*$/;
    for (const base of ["storefront", "My_Shop.DB", "2024-sales", "--x--", "日本語", "a".repeat(60), "a-b--c"]) {
      for (const prefix of ["", "demo-", "averyveryverylongowner-"]) {
        const s = suggestName(base, prefix);
        expect(s).toMatch(re);
        expect(s.length).toBeLessThanOrEqual(MAX_NAME - prefix.length);
      }
    }
  });
  it("前置きを含めて 32 文字に収まるよう、ベース名を切る", () => {
    expect(suggestName("a".repeat(40), "demo-", rand(0))).toBe("a".repeat(23) + "-aaa");
  });
  it("切った末尾がハイフンなら落とす", () => {
    const s = suggestName("abcdefghijklmnopqrstuv-xyz", "demo-", rand(0));
    expect(s).toBe("abcdefghijklmnopqrstuv-aaa");
  });
  it("数字で始まる・空のベース名でも文字で始める", () => {
    expect(suggestName("2024", "", rand(0))).toBe("b2024-aaa");
    expect(suggestName("", "", rand(0))).toBe("branch-aaa");
  });
  it("前置きが長くて余地が小さいときも成り立つ", () => {
    const s = suggestName("shop", "x".repeat(28) + "-", rand(0));
    expect(s).toMatch(/^[a-z][a-z0-9]*$/);
    expect(s.length).toBeLessThanOrEqual(3);
  });
  it("呼ぶたびに違う候補が出る", () => {
    expect(suggestName("shop", "", rand(0.1, 0.2, 0.3))).not.toBe(suggestName("shop", "", rand(0.5, 0.6, 0.7)));
  });
});

describe("nameProblem", () => {
  it("空は問題にしない（まだ打っていない）", () => {
    expect(nameProblem("demo-", "")).toBeNull();
  });
  it("正しい名前は問題なし", () => {
    expect(nameProblem("demo-", "my-feature")).toBeNull();
    expect(nameProblem("demo-", "demo-my-feature")).toBeNull();
  });
  it("小文字の英字で始まらなければ start", () => {
    expect(nameProblem("demo-", "1abc")).toBe("start");
    expect(nameProblem("demo-", "Abc")).toBe("start");
    expect(nameProblem("demo-", "-abc")).toBe("start");
  });
  it("大文字・記号・連続や末尾のハイフンは chars", () => {
    expect(nameProblem("demo-", "ab_c")).toBe("chars");
    expect(nameProblem("demo-", "ab--c")).toBe("chars");
    expect(nameProblem("demo-", "abc-")).toBe("chars");
    expect(nameProblem("demo-", "aBc")).toBe("chars");
  });
  it("前置きのあとに打てる長さを超えれば long", () => {
    expect(nameProblem("demo-", "a".repeat(27))).toBeNull();
    expect(nameProblem("demo-", "a".repeat(28))).toBe("long");
    expect(nameProblem("", "a".repeat(33))).toBe("long");
  });
});
