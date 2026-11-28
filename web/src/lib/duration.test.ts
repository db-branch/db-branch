import { describe, expect, it } from "vitest";
import { parseDurationMs, parsePositiveDuration } from "./duration";

describe("parseDurationMs", () => {
  it("parses Go-style durations into milliseconds and rejects invalid strings", () => {
    expect(parseDurationMs("4h")).toBe(4 * 3_600_000);
    expect(parseDurationMs("1h30m")).toBe(5_400_000);
    expect(parseDurationMs("1.5h")).toBe(5_400_000);
    expect(parseDurationMs("30s")).toBe(30_000);
    expect(parseDurationMs("0s")).toBe(0);
    expect(parseDurationMs("0h0m")).toBe(0);
    for (const bad of ["", "bad", "1h extra", "-1h", "1d", "4"]) {
      expect(parseDurationMs(bad)).toBeNull();
    }
  });
});

describe("parsePositiveDuration", () => {
  it("rejects zero or invalid durations and normalizes days when allowDays is set", () => {
    for (const bad of ["", "0h", "0m", "0s", "0h0m", "0h0m0s", "0.0h", "0.000h", "bad"]) {
      expect(parsePositiveDuration(bad)).toBeNull();
      expect(parsePositiveDuration(bad, { allowDays: true })).toBeNull();
    }
    expect(parsePositiveDuration("1d")).toBeNull();
    expect(parsePositiveDuration("0d", { allowDays: true })).toBeNull();
    expect(parsePositiveDuration("9".repeat(400) + "d", { allowDays: true })).toBeNull();
    expect(parsePositiveDuration("2d", { allowDays: true })).toBe("48h");
    expect(parsePositiveDuration(" 2D ", { allowDays: true, caseInsensitive: true })).toBe("48h");
    expect(parsePositiveDuration(" 2D ", { allowDays: true })).toBeNull();
    expect(parsePositiveDuration(" 1h30m ")).toBe("1h30m");
    expect(parsePositiveDuration("0h1m", { allowDays: true })).toBe("0h1m");
  });
});
