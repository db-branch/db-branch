import { describe, expect, it } from "vitest";
import { cellText, isRunKey, resultSummary } from "./sqlrun";

describe("cellText", () => {
  it("shows NULL for a SQL null and the text otherwise", () => {
    expect(cellText(null)).toBe("NULL");
    expect(cellText("")).toBe("");
    expect(cellText("a b")).toBe("a b");
  });
});

describe("resultSummary", () => {
  it("counts the rows and the time", () => {
    expect(resultSummary({ columns: ["a"], rows: [["1"], ["2"]], millis: 12 })).toBe("2 rows · 12 ms");
    expect(resultSummary({ columns: ["a"], rows: [["1"]], millis: 3 })).toBe("1 row · 3 ms");
    expect(resultSummary({ columns: ["a"], rows: [], millis: 0 })).toBe("0 rows · 0 ms");
  });
  it("says how many rows a statement without rows changed", () => {
    expect(resultSummary({ affected: 5, millis: 7 })).toBe("5 rows affected · 7 ms");
    expect(resultSummary({ affected: 1, millis: 7 })).toBe("1 row affected · 7 ms");
  });
  it("says when the rows were cut", () => {
    expect(resultSummary({ columns: ["a"], rows: [["1"]], truncated: true, millis: 5 })).toBe("1 row (cut) · 5 ms");
  });
});

describe("isRunKey", () => {
  it("is Ctrl or Command with Enter", () => {
    expect(isRunKey({ key: "Enter", ctrlKey: true, metaKey: false })).toBe(true);
    expect(isRunKey({ key: "Enter", ctrlKey: false, metaKey: true })).toBe(true);
    expect(isRunKey({ key: "Enter", ctrlKey: false, metaKey: false })).toBe(false);
    expect(isRunKey({ key: "a", ctrlKey: true, metaKey: false })).toBe(false);
  });
});
