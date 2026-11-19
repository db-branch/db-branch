import { describe, expect, it } from "vitest";
import headersRaw from "../../demo-worker/_headers?raw";

interface HeaderBlock {
  pattern: string;
  headers: Array<{ name: string; value: string }>;
}

function parseCloudflareHeaders(raw: string): HeaderBlock[] {
  const blocks: HeaderBlock[] = [];
  let current: HeaderBlock | null = null;

  for (const line of raw.split(/\r?\n/)) {
    const trimmed = line.trim();
    if (trimmed === "" || trimmed.startsWith("#")) continue;

    if (!/^\s/.test(line)) {
      current = { pattern: trimmed, headers: [] };
      blocks.push(current);
      continue;
    }

    if (!current) {
      throw new Error(`Header line without preceding path pattern: ${line}`);
    }
    const colon = trimmed.indexOf(":");
    if (colon <= 0) {
      throw new Error(`Invalid header line: ${line}`);
    }
    const name = trimmed.slice(0, colon).trim();
    const value = trimmed.slice(colon + 1).trim();
    current.headers.push({ name, value });
  }

  return blocks;
}

function matchesPattern(pattern: string, requestPath: string): boolean {
  const pathname = requestPath.split("?")[0];
  const splatCount = (pattern.match(/\*/g) ?? []).length;
  if (splatCount > 1) {
    throw new Error(`Cloudflare _headers allows at most one splat per pattern: ${pattern}`);
  }
  const escaped = pattern.replace(/[.+?^${}()|[\]\\]/g, "\\$&").replace(/\*/g, ".*");
  return new RegExp(`^${escaped}$`).test(pathname);
}

function resolveHeaders(blocks: HeaderBlock[], requestPath: string): Record<string, string[]> {
  const result: Record<string, string[]> = {};
  for (const block of blocks) {
    if (!matchesPattern(block.pattern, requestPath)) continue;
    for (const { name, value } of block.headers) {
      const key = name.toLowerCase();
      result[key] ??= [];
      result[key].push(value);
    }
  }
  return result;
}

function expectSecurityHeaders(headers: Record<string, string[]>): void {
  expect(headers["content-security-policy"]).toHaveLength(1);
  expect(headers["content-security-policy"][0]).toContain("default-src 'self'");
  expect(headers["x-content-type-options"]).toEqual(["nosniff"]);
  expect(headers["referrer-policy"]).toEqual(["strict-origin-when-cross-origin"]);
}

describe("demo-worker _headers", () => {
  const blocks = parseCloudflareHeaders(headersRaw);

  it("applies X-Robots-Tag: noindex to /v1/* paths without duplicating it", () => {
    for (const path of [
      "/v1/backups/xxx.tar.zst?node=node-1",
      "/v1/branches",
      "/v1/",
    ]) {
      const headers = resolveHeaders(blocks, path);
      expect(headers["x-robots-tag"]).toEqual(["noindex"]);
      expectSecurityHeaders(headers);
    }
  });

  it("keeps SPA routes indexable (no X-Robots-Tag) with security headers intact", () => {
    for (const path of ["/", "/admin", "/admin/bases", "/account"]) {
      const headers = resolveHeaders(blocks, path);
      expect(headers["x-robots-tag"]).toBeUndefined();
      expectSecurityHeaders(headers);
    }
  });

  it("preserves /assets/* Cache-Control and security headers without X-Robots-Tag", () => {
    const headers = resolveHeaders(blocks, "/assets/index-abc123.js");
    expect(headers["cache-control"]).toEqual(["public, max-age=31536000, immutable"]);
    expect(headers["x-robots-tag"]).toBeUndefined();
    expectSecurityHeaders(headers);
  });
});
