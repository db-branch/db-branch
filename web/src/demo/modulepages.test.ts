import { describe, expect, it } from "vitest";
import { modulePage } from "./modulepages";

const now = Date.parse("2026-11-12T03:00:00Z");

describe("modulePage", () => {
  it("shows sample users for the auth module", () => {
    const html = modulePage("auth", "/users", now);
    expect(html).toContain("<h1>Users</h1>");
    expect(html).toContain("alex@example.com");
    expect(html).toContain("2026-11-12 02:58 UTC");
  });
  it("shows a sample audit log for the audit module", () => {
    const html = modulePage("audit", "/log", now);
    expect(html).toContain("branch.create");
    expect(html).toContain("<th>Outcome</th>");
  });
  it("has no script and loads nothing", () => {
    const html = modulePage("auth", "/users", now);
    expect(html).not.toMatch(/<script|<link|src=|href=|@import|url\(/i);
  });
  it("says so for a page it has no sample for, escaping the name", () => {
    const html = modulePage("<b>x</b>", "/nothing", now);
    expect(html).not.toContain("<b>");
    expect(html).toContain("&lt;b&gt;");
  });
});
