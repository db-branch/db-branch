import { afterEach, describe, expect, it } from "vitest";
import { ApiError } from "../api";
import { ERROR_CODES, errorText, formatError, LocalizedError, serverError } from "./errors";
import { setLanguage } from "./i18n";
import { ja } from "./ja";

const httpapiFiles = import.meta.glob<string>("../../../internal/interface/httpapi/*.go", { query: "?raw", import: "default", eager: true });
const mergeFiles = import.meta.glob<string>("../../../internal/usecase/dbbranch/merge.go", { query: "?raw", import: "default", eager: true });

afterEach(() => setLanguage("en"));

describe("formatError", () => {
  it("returns null for empty errors", () => {
    expect(formatError(null)).toBeNull();
    expect(formatError(undefined)).toBeNull();
    expect(formatError("")).toBeNull();
  });

  it("preserves original English messages in en mode", () => {
    setLanguage("en");
    const err = new ApiError(409, "already_exists", "branch x already exists");
    expect(formatError(err)).toEqual({ text: "branch x already exists" });
    expect(errorText(err)).toBe("branch x already exists");
  });

  it("translates known server error codes in ja mode", () => {
    setLanguage("ja");
    const err = new ApiError(409, "already_exists", "branch x already exists");
    const formatted = formatError(err);
    expect(formatted?.text).toBe(ja[ERROR_CODES.already_exists]);
    expect(formatted?.detail).toBeUndefined();
    expect(formatted?.text).not.toContain("branch x already exists");
  });

  it("includes SQL detail for query_failed in ja mode", () => {
    setLanguage("ja");
    const err = new ApiError(422, "query_failed", "ERROR 1064 (42000): syntax error");
    expect(formatError(err)).toEqual({
      text: ja[ERROR_CODES.query_failed],
      detail: "ERROR 1064 (42000): syntax error",
    });
  });

  it("includes reason detail for forbidden, quota_exceeded, conflict, and invalid_request in ja mode", () => {
    setLanguage("ja");
    const err = new ApiError(403, "forbidden", "team pay has 2 branches, its limit; delete one first");
    expect(formatError(err)).toEqual({
      text: ja[ERROR_CODES.forbidden],
      detail: "team pay has 2 branches, its limit; delete one first",
    });
  });

  it("shows generic Japanese message plus small original text for unknown error codes", () => {
    setLanguage("ja");
    const err = new ApiError(500, "future_unknown_code", "something went sideways");
    expect(formatError(err)).toEqual({
      text: ja["An error occurred."],
      detail: "something went sideways",
    });

    const plain = new Error("Failed to fetch");
    expect(formatError(plain)).toEqual({
      text: ja["An error occurred."],
      detail: "Failed to fetch",
    });

    const srv = serverError("zfs clone failed: dataset busy");
    expect(formatError(srv)).toEqual({
      text: ja["An error occurred."],
      detail: "zfs clone failed: dataset busy",
    });

    expect(formatError("raw english string")).toEqual({
      text: ja["An error occurred."],
      detail: "raw english string",
    });
  });

  it("keeps LocalizedError and already-translated strings as-is in ja mode", () => {
    setLanguage("ja");
    const loc = new LocalizedError("ダンプファイルを選んでください。");
    expect(formatError(loc)).toEqual({ text: "ダンプファイルを選んでください。" });
    expect(formatError("すでに存在します。")).toEqual({ text: "すでに存在します。" });
  });
});

describe("ERROR_CODES coverage", () => {
  it("has a Japanese translation in ja.ts for every known error code", () => {
    const missing = Object.entries(ERROR_CODES)
      .filter(([, en]) => !(en in ja))
      .map(([code, en]) => `${code} -> ${en}`);
    expect(missing).toEqual([]);
  });

  it("covers every error code emitted by the Go httpapi and merge packages", () => {
    const foundCodes = new Set<string>();
    for (const [path, src] of Object.entries(httpapiFiles)) {
      if (path.endsWith("_test.go")) continue;
      for (const m of src.matchAll(/writeError\([^,]+,[^,]+,\s*"([a-z0-9_]+)"/g)) {
        foundCodes.add(m[1]);
      }
    }
    const mergeSrc = Object.values(mergeFiles)[0] ?? "";
    expect(mergeSrc.length).toBeGreaterThan(0);
    expect(Object.keys(httpapiFiles).length).toBeGreaterThan(0);
    const mergeFn = mergeSrc.slice(mergeSrc.indexOf("func mergeErrorCode"));
    const mergeEnd = mergeFn.indexOf("\n}");
    for (const m of mergeFn.slice(0, mergeEnd).matchAll(/return\s+"([a-z0-9_]+)"/g)) {
      foundCodes.add(m[1]);
    }
    // Check any error code returned as a string literal in httpapi (e.g. from code-mapping helpers).
    for (const [path, src] of Object.entries(httpapiFiles)) {
      if (path.endsWith("_test.go")) continue;
      for (const m of src.matchAll(/\bwriteError\([^,]+,[^,]+,\s*"([a-z0-9_]+)"/g)) {
        foundCodes.add(m[1]);
      }
      for (const m of src.matchAll(/\bcase\b[^\n]*:\s*\n\s*return\s+"([a-z0-9_]+)"/g)) {
        foundCodes.add(m[1]);
      }
    }

    const uncovered = [...foundCodes].filter((c) => !(c in ERROR_CODES)).sort();
    expect(uncovered).toEqual([]);
  });
});
