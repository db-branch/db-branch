import { describe, expect, it } from "vitest";
import { shouldRestore } from "./persist";

describe("shouldRestore", () => {
  it("starts afresh when nothing was saved", () => {
    expect(shouldRestore(null, "navigate")).toBe(false);
  });
  it("keeps the data when the person moved to another page of the demo", () => {
    expect(shouldRestore('{"keep":false}', "navigate")).toBe(true);
    expect(shouldRestore('{"keep":false}', "back_forward")).toBe(true);
    expect(shouldRestore("{}", undefined)).toBe(true);
  });
  it("starts over on a reload", () => {
    expect(shouldRestore('{"keep":false}', "reload")).toBe(false);
    expect(shouldRestore("not json", "reload")).toBe(false);
  });
  it("keeps the data over the reload a language switch makes", () => {
    expect(shouldRestore('{"keep":true}', "reload")).toBe(true);
  });
});
