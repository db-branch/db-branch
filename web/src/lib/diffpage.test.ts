import { describe, expect, it } from "vitest";
import { diffFrame, showDiff } from "./diffpage";

const src = (search: string, lang: string) => (diffFrame(search, lang) as { src: string }).src;

describe("showDiff", () => {
  it("is for people the diff module's API is open to, when the module has its page", () => {
    const caps = { pages: [{ module: "diff", path: "/ui/", title: "Diff (beta)" }] };
    expect(showDiff(caps, () => true)).toBe(true);
    expect(showDiff(caps, (a) => a !== "ext.diff.read")).toBe(false);
    expect(showDiff({ pages: [{ module: "auth", path: "/users", title: "Users" }] }, () => true)).toBe(false);
    expect(showDiff(undefined, () => true)).toBe(false);
  });
});

describe("diffFrame", () => {
  it("loads the module's own page and passes on the two branches from the address", () => {
    expect(src("?a=alice-x&b=alice-y&other=1", "ja")).toBe("/v1/ext/diff/ui/?lang=ja&a=alice-x&b=alice-y");
  });
  it("passes on only what is there, and escapes it", () => {
    expect(src("", "en")).toBe("/v1/ext/diff/ui/?lang=en");
    expect(src("?a=x%26y", "en")).toBe("/v1/ext/diff/ui/?lang=en&a=x%26y");
  });
});
