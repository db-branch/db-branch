import { describe, expect, it } from "vitest";
import redirectsRaw from "../../demo-worker/_redirects?raw";
import { tryLocallyUrl } from "./tryurl";

// Cloudflare _redirects: "source destination [status]", one rule per line, # for comments.
const rules = redirectsRaw
  .split(/\r?\n/)
  .map((l) => l.trim())
  .filter((l) => l !== "" && !l.startsWith("#"))
  .map((l) => {
    const [from, to, status] = l.split(/\s+/);
    return { from, to, status: Number(status) };
  });
const rule = (from: string) => rules.find((r) => r.from === from);

describe("demo-worker _redirects (paths the analytics can count)", () => {
  it("sends /from/readme to the demo itself, as a temporary redirect", () => {
    expect(rule("/from/readme")).toEqual({ from: "/from/readme", to: "/", status: 302 });
  });
  it("sends the 'run it locally' button to getting-started, English and Japanese", () => {
    expect(rule("/go/getting-started")).toEqual({ from: "/go/getting-started", to: "https://db-branch.com/docs/getting-started/", status: 302 });
    expect(rule("/go/getting-started-ja")).toEqual({ from: "/go/getting-started-ja", to: "https://db-branch.com/ja/docs/getting-started/", status: 302 });
  });
  it("has a rule for every link the banner can show", () => {
    for (const lang of ["en", "ja"] as const) expect(rule(tryLocallyUrl(lang))).toBeDefined();
  });
  it("has nothing else (every rule is a 302, no query strings in sources)", () => {
    expect(rules).toHaveLength(3);
    for (const r of rules) {
      expect(r.status).toBe(302);
      expect(r.from).not.toContain("?");
    }
  });
});
