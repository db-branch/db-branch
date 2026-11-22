import { describe, expect, it } from "vitest";
import type { Setting } from "../api";
import { editedValue, hasChange, initialValue, settingsChange } from "./settingsform";

const mk = (over: Partial<Setting>): Setting => ({ key: "k", label: "K", type: "string", default: "", value: "", ...over });
const text = mk({ key: "note", type: "text", default: "d", value: "d" });
const key = mk({ key: "license.key", type: "secret", secret_set: true });

describe("settingsform", () => {
  it("text型は末尾の改行やスペースをそのまま保ち、空にすると既定値に戻る", () => {
    expect(editedValue(text, "line1\n")).toBe("line1\n");
    expect(editedValue(text, "a ")).toBe("a ");
    expect(editedValue(text, "")).toBe("d");
  });
  it("secret型は入力のまま保ち、空でも既定値に置き換えない", () => {
    expect(editedValue(key, " abc ")).toBe(" abc ");
    expect(editedValue(mk({ type: "secret", default: "dbbranch" }), "")).toBe("");
  });
  it("secret型は設定済みでも空欄から始まる", () => {
    expect(initialValue(key)).toBe("");
    expect(initialValue(text)).toBe("d");
  });
  it("変更がなければ何も送らない", () => {
    const c = settingsChange([text, key], {}, []);
    expect(c).toEqual({ values: {}, clear: [] });
    expect(hasChange(c)).toBe(false);
  });
  it("text型の変更は改行つきで送り、secret型は入力したときだけ trim して送る", () => {
    const c = settingsChange([text, key], { note: "x\n", "license.key": "  NEW  " }, []);
    expect(c.values).toEqual({ note: "x\n", "license.key": "NEW" });
    expect(hasChange(c)).toBe(true);
  });
  it("空白だけを入力した secret は変更にならない", () => {
    expect(settingsChange([key], { "license.key": "   " }, [])).toEqual({ values: {}, clear: [] });
  });
  it("secret型は明示クリアのときだけ clear に入り、新しい値の入力が優先される", () => {
    expect(settingsChange([key], {}, ["license.key"])).toEqual({ values: {}, clear: ["license.key"] });
    expect(settingsChange([key], { "license.key": "N" }, ["license.key"])).toEqual({ values: { "license.key": "N" }, clear: [] });
  });
});
