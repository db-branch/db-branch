import { describe, expect, it } from "vitest";
import type { Checkpoint } from "../api";
import { checkpointNameProblem, hasHistory, MAX_CHECKPOINT_NAME, newestFirst, notes, savedLabel } from "./checkpoints";

const cp = (name: string, at: string, over: Partial<Checkpoint> = {}): Checkpoint => ({ name, created_at: at, size: 0, line: 0, auto: false, current: false, ...over });

describe("checkpointNameProblem", () => {
  it("何も入力していなければ問題なし(名前は省略できる)", () => {
    expect(checkpointNameProblem("")).toBeNull();
  });
  it("小文字・数字・ハイフンだけを受け付ける", () => {
    for (const ok of ["a", "before-migration", "20261112-153000", "v1-2"]) expect(checkpointNameProblem(ok)).toBeNull();
    for (const bad of ["A", "a_b", "a b", "a/b", "日本語", "-a", "a-"]) expect(checkpointNameProblem(bad)).toBe("chars");
  });
  it("長すぎる名前は long", () => {
    expect(checkpointNameProblem("a".repeat(MAX_CHECKPOINT_NAME))).toBeNull();
    expect(checkpointNameProblem("a".repeat(MAX_CHECKPOINT_NAME + 1))).toBe("long");
  });
});

describe("newestFirst", () => {
  it("新しいものを先頭にし、元の配列は変えない", () => {
    const list = [cp("a", "2026-11-12T10:00:00Z"), cp("c", "2026-11-12T12:00:00Z"), cp("b", "2026-11-12T11:00:00Z")];
    expect(newestFirst(list).map((c) => c.name)).toEqual(["c", "b", "a"]);
    expect(list.map((c) => c.name)).toEqual(["a", "c", "b"]);
  });
  it("同時刻は名前の逆順で安定させる", () => {
    const t = "2026-11-12T10:00:00Z";
    expect(newestFirst([cp("a", t), cp("b", t)]).map((c) => c.name)).toEqual(["b", "a"]);
  });
});

describe("hasHistory", () => {
  it("復元で別の線ができていれば真", () => {
    expect(hasHistory([cp("a", "x"), cp("b", "x")])).toBe(false);
    expect(hasHistory([cp("a", "x"), cp("b", "x", { line: 1 })])).toBe(true);
  });
});

describe("notes", () => {
  it("現在地と自動を示す", () => {
    expect(notes(cp("a", "x", { current: true, auto: true }))).toEqual(["current", "auto"]);
    expect(notes(cp("a", "x"))).toEqual([]);
    expect(notes(cp("a", "x", { auto: true }))).toEqual(["auto"]);
  });
});

describe("savedLabel", () => {
  it("名前を付けて保存した結果を短く言う", () => {
    expect(savedLabel("before-migration")).toBe("Saved checkpoint before-migration");
  });
});
