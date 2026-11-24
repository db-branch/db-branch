import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it } from "vitest";
import type { Base, Branch } from "../api";
import { setLanguage } from "../lib/i18n";
import { AnonymousCard } from "./BaseAnonymous";
import { BaseBody } from "./Bases";

const noop = () => {};

const base: Base = {
  name: "storefront",
  description: "Storefront catalog",
  runtime: "postgres@18.6",
  node: "node-1",
  port: 5433,
  status: "ready",
  status_reason: null,
  created_at: "2026-11-01T00:00:00Z",
  latest_snapshot: "s2026112405",
  snapshots: [
    { name: "s2026112405", created_at: "2026-11-24T05:00:00Z", public: true },
    { name: "s2026112305", created_at: "2026-11-23T05:00:00Z", public: false },
  ],
  replicas: {},
  holders: [],
  operation: null,
  anonymous: { max_branches: null, max_ttl: null },
};

const branch: Branch = {
  name: "alex-checkout-fix",
  id: "3c6ef3622f5e5358",
  status: "ready",
  status_reason: null,
  base: "storefront",
  snapshot: "s2026112405",
  runtime: "postgres@18.6",
  node: "node-1",
  created_at: "2026-11-24T06:00:00Z",
  expires_at: "2026-11-25T06:00:00Z",
  reset_at: null,
  storage: { used: 1024, referenced: 2048 },
  connection: { host: "demo.invalid", port: 41001, user: "alex-checkout-fix" },
  proxy: null,
  operation: null,
  owner: "u-alex",
  owner_name: "alex@example.com",
  access: "private",
  checkpoints: true,
};

afterEach(() => {
  setLanguage("en");
});

describe("BaseBody", () => {
  it("shows branch names and links to /admin/branches/... instead of raw hex ids, and wraps snapshots in table-wrap", () => {
    const h = renderToStaticMarkup(<BaseBody base={base} branches={[branch]} nodes={["node-1", "node-2"]} onUpdate={noop} onDelete={noop} />);
    expect(h).toContain("alex-checkout-fix");
    expect(h).toContain('href="/admin/branches/alex-checkout-fix"');
    expect(h).not.toContain("3c6ef3622f5e5358");
    expect(h).toContain('class="table-wrap"');
  });
});

describe("AnonymousCard", () => {
  it("does not leave an empty rate placeholder in Japanese when rate_per_hour is missing", () => {
    setLanguage("ja");
    const h = renderToStaticMarkup(<AnonymousCard base={base} branches={[branch]} />);
    expect(h).not.toMatch(/1 時間に\s*回まで/);
  });
});
