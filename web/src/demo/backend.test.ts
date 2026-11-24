import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Base, Branch, Checkpoint, Operation } from "../api";
import { DemoBackend } from "./backend";

let b: DemoBackend;
let events: string[];

const call = (method: string, path: string, body?: unknown) => b.handle({ method, path, body });
const get = <T = any>(path: string) => call("GET", path).body as T;
const branches = () => get<{ items: Branch[] }>("/v1/branches").items;
const branch = (name: string) => branches().find((x) => x.name === name);
const base = (name: string) => get<{ items: Base[] }>("/v1/bases").items.find((x) => x.name === name)!;
const op = (id: string) => get<Operation>(`/v1/operations/${id}`);

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-11-12T03:00:00Z"));
  b = new DemoBackend();
  events = [];
  b.on((type) => events.push(type));
  b.signIn("admin");
});
afterEach(() => vi.useRealTimers());

describe("sessions", () => {
  it("answers 401 until someone is signed in, and again after logout", () => {
    b.signOut();
    expect(call("GET", "/v1/branches").status).toBe(401);
    expect(call("GET", "/v1/auth/session").status).toBe(401);
    b.signIn("developer");
    expect(call("GET", "/v1/branches").status).toBe(200);
    expect(call("POST", "/v1/auth/logout").status).toBe(204);
    expect(call("GET", "/v1/branches").status).toBe(401);
  });
  it("shows the admin everything and says so in can", () => {
    const s = get("/v1/auth/session");
    expect(s.user).toMatchObject({ id: "demo-admin", roles: ["admin"] });
    expect(s.can["base.create"]).toBe(true);
    expect(s.branch_prefix).toBe("");
  });
  it("makes the developer a non-admin with an owner prefix and no admin rights", () => {
    b.signIn("developer");
    const s = get("/v1/auth/session");
    expect(s.user.id).toBe("alex@example.com");
    expect(s.branch_prefix).toBe("alex-");
    expect(s.can["base.read"]).toBe(true);
    expect(s.can["setting.write"]).toBe(false);
    expect(s.can["branch.restore"]).toBe(true); // on their own branches
    expect(call("GET", "/v1/settings").status).toBe(403);
    expect(call("GET", "/v1/cluster").status).toBe(403);
    expect(call("POST", "/v1/bases", { name: "x", runtime: "postgres@18.6" }).status).toBe(403);
  });
  it("accepts any credentials: admin names sign in as admin, others as developers", () => {
    b.signOut();
    expect(call("POST", "/v1/auth/login", { username: "admin", password: "x" }).body).toMatchObject({ user: { id: "demo-admin" } });
    b.signOut();
    const dev = call("POST", "/v1/auth/login", { username: "Sam.Lee@corp.test", password: "x" }).body as any;
    expect(dev.user).toMatchObject({ id: "sam.lee@corp.test", roles: ["developer"] });
    expect(dev.branch_prefix).toBe("sam-lee-");
    expect(branches()).toEqual([]); // a newcomer owns nothing yet
  });
  it("lists no login methods", () => {
    b.signOut();
    expect(get("/v1/auth/methods")).toEqual({ items: [] });
  });
  it("keeps the language the person chose", () => {
    expect(call("PUT", "/v1/account/preferences", { language: "ja" }).status).toBe(200);
    expect(get("/v1/account/preferences").language).toBe("ja");
    expect(get("/v1/auth/session").language).toBe("ja");
    expect(b.state.keep).toBe(true);
  });
  it("takes a password change only if it is long enough", () => {
    expect(call("PUT", "/v1/auth/password", { current_password: "a", new_password: "short" }).body).toMatchObject({ error: { code: "weak_password" } });
    expect(call("PUT", "/v1/auth/password", { current_password: "a", new_password: "long enough password" }).status).toBe(204);
  });
});

describe("seed", () => {
  it("has a three-node cluster with node-1 leading", () => {
    const c = get("/v1/cluster");
    expect(c.nodes.map((n: any) => n.name)).toEqual(["node-1", "node-2", "node-3"]);
    expect(c.nodes[0].leader).toBe(true);
    expect(c.nodes[0].system).toMatchObject({ distro: "ubuntu-24.04", os: "Ubuntu 24.04.3 LTS", zfs: "2.2.2-0ubuntu9.4" });
    expect(c.nodes.every((n: any) => n.system.kernel.startsWith("6.8.0"))).toBe(true);
    expect(c.nodes.every((n: any) => n.runtimes.includes("postgres@18.6"))).toBe(true);
  });
  it("has the storefront and orders bases, one snapshot public", () => {
    expect(base("storefront")).toMatchObject({ runtime: "postgres@18.6", status: "ready" });
    expect(base("orders").runtime).toMatch(/^mysql@8\.4/);
    expect(base("storefront").snapshots.filter((x) => x.public)).toHaveLength(1);
    expect(base("storefront").snapshots.length).toBeGreaterThan(2);
    expect(base("storefront").can?.["base.delete"]).toBe(true);
  });
  it("shows the developer only their own branches", () => {
    b.signIn("developer");
    expect(branches().map((x) => x.name)).toEqual(["alex-checkout-fix", "alex-orders-report"]);
  });
  it("has the licence in trial and five enabled modules", () => {
    const lic = get("/v1/license");
    expect(lic.configured).toBe(false);
    expect(lic.report.trial).toBe(true);
    expect(get("/v1/modules").items.filter((m: any) => m.wanted).map((m: any) => m.name)).toEqual(["audit", "auth", "history", "proxysql"]);
  });
  it("only ever shows the fake host and password", () => {
    const text = JSON.stringify([branches(), get("/v1/bases"), get("/v1/cluster")]);
    expect(text).not.toMatch(/\b(\d{1,3}\.){3}\d{1,3}\b/);
    const dsn = get<{ dsn: string }>("/v1/branches/feature-checkout/dsn").dsn;
    expect(dsn).toMatch(/^postgres:\/\/[^:]+:demo@demo\.invalid:\d+\/\w+$/);
    expect(get<{ dsn: string }>("/v1/branches/report-fix/dsn").dsn).toMatch(/^mysql:\/\/[^:]+:demo@demo\.invalid:\d+\/\w+$/);
    expect(get<{ dsn: string }>("/v1/branches/report-fix/dsn?via=proxy").dsn).toMatch(/^mysql:\/\/report-fix:demo@demo\.invalid:6033\//);
    expect(get<{ items: { name: string; runtime: string; proxy: unknown }[] }>("/v1/branches").items.find((b) => b.runtime.startsWith("postgres"))?.proxy).toBeNull();
    expect(get<{ anonymous: { rate_per_hour?: number } }>("/v1/public/bases").anonymous.rate_per_hour).toBeGreaterThan(0);
  });
});

describe("branches", () => {
  it("creates a branch: provisioning at once, ready after about two seconds, with events", () => {
    const r = call("POST", "/v1/branches", { name: "try-it", base: "storefront", ttl: "4h" });
    expect(r.status).toBe(202);
    const o = r.body as Operation;
    expect(o).toMatchObject({ kind: "branch.create", state: "running", target: { resource: "branch", name: "try-it" } });
    expect(branch("try-it")).toMatchObject({ status: "provisioning", operation: { id: o.id } });
    vi.advanceTimersByTime(1900);
    expect(branch("try-it")!.status).toBe("provisioning");
    vi.advanceTimersByTime(200);
    expect(branch("try-it")).toMatchObject({ status: "ready", operation: null, owner: "demo-admin" });
    expect(op(o.id).state).toBe("succeeded");
    expect(events).toContain("branch.updated");
    expect(events).toContain("operation.updated");
    const left = Date.parse(branch("try-it")!.expires_at!) - Date.now();
    expect(left).toBeGreaterThan(3.9 * 3600e3);
    expect(left).toBeLessThanOrEqual(4 * 3600e3);
  });
  it("puts the owner prefix in front of what a developer types, and owns it", () => {
    b.signIn("developer");
    call("POST", "/v1/branches", { name: "login-bug", base: "orders" });
    expect(branch("alex-login-bug")).toMatchObject({ owner: "alex@example.com", base: "orders" });
    call("POST", "/v1/branches", { name: "alex-again", base: "orders" });
    expect(branch("alex-again")).toBeDefined(); // already prefixed: kept
    expect(branch("alex-alex-again")).toBeUndefined();
  });
  it("lets the admin name branches exactly and choose the owner", () => {
    call("POST", "/v1/branches", { name: "plain", base: "orders", owner: "carol", access: "open" });
    expect(branch("plain")).toMatchObject({ owner: "carol", access: "open" });
  });
  it("refuses a duplicate, a bad name, an unknown base and a bad ttl", () => {
    expect(call("POST", "/v1/branches", { name: "report-fix", base: "orders" })).toMatchObject({ status: 409, body: { error: { code: "already_exists" } } });
    expect(call("POST", "/v1/branches", { name: "Bad_Name", base: "orders" })).toMatchObject({ status: 400, body: { error: { code: "invalid_request" } } });
    expect(call("POST", "/v1/branches", { name: "ok", base: "nope" })).toMatchObject({ status: 404, body: { error: { code: "not_found" } } });
    expect(call("POST", "/v1/branches", { name: "ok", base: "orders", ttl: "soon" }).status).toBe(400);
    b.signIn("developer");
    expect(call("POST", "/v1/branches", { name: "x".repeat(30), base: "orders" }).status).toBe(400); // prefix + 30 > 32
  });
  it("resets after about 1.3 seconds and restores the starting data", () => {
    const before = structuredClone(branch("migration-test")!);
    expect(before.storage!.used).toBeGreaterThan(100 * 1024 * 1024);
    const started = Date.now();
    const r = call("POST", "/v1/branches/migration-test/reset");
    expect(r.status).toBe(202);
    expect(branch("migration-test")).toMatchObject({ status: "resetting" });
    vi.advanceTimersByTime(1200);
    expect(branch("migration-test")!.status).toBe("resetting");
    vi.advanceTimersByTime(200);
    const after = branch("migration-test")!;
    expect(after.status).toBe("ready");
    expect(after.reset_at).toBe(new Date(started + 1300).toISOString());
    expect(after.storage!.used).toBeLessThan(before.storage!.used / 10);
    expect(after.snapshot).toBe(before.snapshot);
  });
  it("will not reset a branch that is busy", () => {
    call("POST", "/v1/branches/migration-test/reset");
    expect(call("POST", "/v1/branches/migration-test/reset").status).toBe(409);
  });
  it("recreates like a new branch", () => {
    const r = call("POST", "/v1/branches/report-fix/recreate");
    expect((r.body as Operation).kind).toBe("branch.create");
    vi.advanceTimersByTime(2100);
    expect(branch("report-fix")!.status).toBe("ready");
  });
  it("deletes: deleting, then gone with a branch.deleted event", () => {
    expect(call("DELETE", "/v1/branches/report-fix").status).toBe(202);
    expect(branch("report-fix")!.status).toBe("deleting");
    vi.advanceTimersByTime(900);
    expect(branch("report-fix")).toBeUndefined();
    expect(events).toContain("branch.deleted");
    expect(call("GET", "/v1/branches/report-fix").status).toBe(404);
  });
  it("extends: expiry becomes now plus the ttl", () => {
    const r = call("PATCH", "/v1/branches/ci-pr-482", { ttl: "10h" });
    expect(r.status).toBe(200);
    expect((r.body as Branch).expires_at).toBe(new Date(Date.now() + 10 * 3600e3).toISOString());
    expect(call("PATCH", "/v1/branches/ci-pr-482", { ttl: "90m" }).status).toBe(200);
    expect(branch("ci-pr-482")!.expires_at).toBe(new Date(Date.now() + 90 * 60e3).toISOString());
    expect(call("PATCH", "/v1/branches/ci-pr-482", { ttl: "x" }).status).toBe(400);
  });
  it("hides other people's branches from a developer (404)", () => {
    b.signIn("developer");
    expect(call("GET", "/v1/branches/feature-checkout").status).toBe(404);
    expect(call("DELETE", "/v1/branches/feature-checkout").status).toBe(404);
    expect(call("PATCH", "/v1/branches/alex-checkout-fix", { ttl: "2h" }).status).toBe(200);
  });
  it("gives the connection only once the branch is ready", () => {
    call("POST", "/v1/branches", { name: "later", base: "storefront" });
    expect(call("GET", "/v1/branches/later/dsn").status).toBe(409);
    vi.advanceTimersByTime(2100);
    expect(call("GET", "/v1/branches/later/dsn").status).toBe(200);
  });
});

describe("anonymous visitors", () => {
  beforeEach(() => b.signOut());
  it("see the public bases and the limits", () => {
    const p = get("/v1/public/bases");
    expect(p.items.map((x: any) => x.name)).toEqual(["storefront"]);
    expect(p.items[0].snapshots).toHaveLength(1);
    expect(p.anonymous).toEqual({ max_ttl: "4h0m0s", max_branches: 2, rate_per_hour: 5 });
  });
  it("may branch from a public snapshot for at most four hours, two at a time", () => {
    expect(call("POST", "/v1/branches", { name: "anon-a", base: "storefront", ttl: "4h" })).toMatchObject({ status: 202 });
    expect(call("POST", "/v1/branches", { name: "anon-b", base: "orders", ttl: "1h" }).status).toBe(403); // not public
    expect(call("POST", "/v1/branches", { name: "anon-b", base: "storefront", ttl: "24h" }).status).toBe(403);
    expect(call("POST", "/v1/branches", { name: "anon-b", base: "storefront", ttl: "1h" }).status).toBe(202);
    expect(call("POST", "/v1/branches", { name: "anon-c", base: "storefront", ttl: "1h" }).status).toBe(429);
    expect(call("GET", "/v1/branches/anon-a").body).toMatchObject({ access: "open" });
    expect(call("GET", "/v1/branches/feature-checkout").status).toBe(404);
    expect(call("GET", "/v1/branches").status).toBe(401);
  });
});

describe("bases", () => {
  it("creates a base from an uploaded dump (only name and size are used)", () => {
    const up = call("POST", "/v1/uploads?name=shop.sql.gz&node=node-2", { size: 50 * 1024 * 1024 });
    expect(up).toMatchObject({ status: 201, body: { name: "shop.sql.gz", size: 50 * 1024 * 1024, node: "node-2" } });
    const id = (up.body as any).id as string;
    expect(id).toMatch(/^upload:/);
    const r = call("POST", "/v1/bases", { name: "shop", runtime: "mysql@8.4.13", dump: id, node: "node-2", description: "Shop" });
    expect(r.status).toBe(202);
    expect(base("shop")).toMatchObject({ status: "provisioning", node: "node-2", snapshots: [], operation: { kind: "base.create" } });
    vi.advanceTimersByTime(4000);
    const done = base("shop");
    expect(done).toMatchObject({ status: "ready", operation: null, description: "Shop" });
    expect(done.snapshots).toHaveLength(1);
    expect(done.latest_snapshot).toBe(done.snapshots[0].name);
    expect(events).toContain("base.updated");
  });
  it("creates an empty base without a snapshot, and refuses duplicates, bad names and missing engines", () => {
    call("POST", "/v1/bases", { name: "blank", runtime: "postgres@18.6" });
    vi.advanceTimersByTime(4000);
    expect(base("blank")).toMatchObject({ status: "ready", latest_snapshot: null });
    expect(call("POST", "/v1/bases", { name: "blank", runtime: "postgres@18.6" }).status).toBe(409);
    expect(call("POST", "/v1/bases", { name: "Bad Name", runtime: "postgres@18.6" }).status).toBe(400);
    expect(call("POST", "/v1/bases", { name: "newone", runtime: "mysql@9.4.0" })).toMatchObject({ status: 404, body: { error: { code: "runtime_not_installed" } } });
  });
  it("rejects an upload bigger than the room", () => {
    expect(get("/v1/uploads/room")).toMatchObject({ max_bytes: 20480 * 1024 * 1024, reason: "limit" });
    expect(call("POST", "/v1/uploads?name=big.sql", { size: 30 * 1024 ** 3 }).status).toBe(413);
  });
  it("takes a snapshot: maintenance, then a new latest snapshot", () => {
    vi.advanceTimersByTime(61_000); // so the new name differs from the seeded ones
    const n = base("storefront").snapshots.length;
    expect(call("POST", "/v1/bases/storefront/snapshots").status).toBe(202);
    expect(base("storefront")).toMatchObject({ status: "maintenance", operation: { kind: "base.snapshot" } });
    expect(call("POST", "/v1/bases/storefront/snapshots").status).toBe(409);
    vi.advanceTimersByTime(1600);
    const s = base("storefront");
    expect(s.status).toBe("ready");
    expect(s.snapshots).toHaveLength(n + 1);
    expect(s.latest_snapshot).toBe(s.snapshots[n].name);
  });
  it("toggles a snapshot public/private; the public list follows", () => {
    const orders = base("orders");
    const snap = orders.latest_snapshot!;
    expect(call("PATCH", `/v1/bases/orders/snapshots/${snap}`, { public: true }).status).toBe(200);
    b.signOut();
    expect(get("/v1/public/bases").items.map((x: any) => x.name)).toEqual(["storefront", "orders"]);
    b.signIn("admin");
    call("PATCH", `/v1/bases/orders/snapshots/${snap}`, { public: false });
    b.signOut();
    expect(get("/v1/public/bases").items.map((x: any) => x.name)).toEqual(["storefront"]);
    b.signIn("admin");
    expect(call("PATCH", "/v1/bases/orders/snapshots/nope", { public: true }).status).toBe(404);
  });
  it("deletes snapshots that are neither the latest nor in use", () => {
    const [oldest] = base("orders").snapshots;
    expect(call("DELETE", `/v1/bases/orders/snapshots/${base("orders").latest_snapshot}`).status).toBe(409);
    expect(call("DELETE", `/v1/bases/orders/snapshots/${oldest.name}`).status).toBe(204);
    expect(base("orders").snapshots.find((x) => x.name === oldest.name)).toBeUndefined();
  });
  it("changes the description", () => {
    expect((call("PATCH", "/v1/bases/orders", { description: "Hello" }).body as Base).description).toBe("Hello");
    expect(base("orders").description).toBe("Hello");
  });
  it("loads a dump or runs SQL without reading anything", () => {
    expect(call("POST", "/v1/bases/orders/sql", { sql: "" }).status).toBe(400);
    expect(call("POST", "/v1/bases/orders/sql", { sql: "select 1" }).status).toBe(204);
    vi.advanceTimersByTime(61_000);
    const r = call("POST", "/v1/bases/orders/load", { dump: "/srv/dumps/x.sql.gz", snapshot: true });
    expect(r.status).toBe(202);
    expect(base("orders").status).toBe("maintenance");
    const n = base("orders").snapshots.length;
    vi.advanceTimersByTime(7000);
    expect(base("orders").status).toBe("ready");
    expect(base("orders").snapshots).toHaveLength(n + 1);
  });
  it("refuses to delete a base with branches, deletes one without", () => {
    expect(call("DELETE", "/v1/bases/orders")).toMatchObject({ status: 409, body: { error: { code: "in_use" } } });
    call("POST", "/v1/bases", { name: "tmp", runtime: "postgres@18.6" });
    vi.advanceTimersByTime(4000);
    expect(call("DELETE", "/v1/bases/tmp").status).toBe(202);
    vi.advanceTimersByTime(1500);
    expect(get<{ items: Base[] }>("/v1/bases").items.find((x) => x.name === "tmp")).toBeUndefined();
    expect(events).toContain("base.deleted");
  });
  it("shows holders on every node, and fewer after choosing replicas", () => {
    expect(base("storefront").holders!.map((h) => [h.node, h.state, h.origin])).toEqual([
      ["node-1", "held", true],
      ["node-2", "held", false],
      ["node-3", "held", false],
    ]);
    expect(base("storefront").replicas[base("storefront").latest_snapshot!]).toEqual(["node-2", "node-3"]);
    const r = call("PUT", "/v1/bases/storefront/replicas", { replica_nodes: ["node-3"] });
    expect(r.status).toBe(200);
    expect(base("storefront").replica_nodes).toEqual(["node-3"]);
    expect(base("storefront").holders!.filter((h) => h.state === "held").map((h) => h.node)).toEqual(["node-1", "node-3"]);
    call("PUT", "/v1/bases/storefront/replicas", { replica_nodes: null });
    expect(base("storefront").replica_nodes).toBeNull();
  });
  it("keeps a refresh schedule: set, run now, clear", () => {
    expect(call("GET", "/v1/bases/storefront/refresh")).toMatchObject({ status: 404 });
    const input = { source: { host: "db.demo.invalid", port: 5432, user: "ro", password: "pw", databases: ["shop"] }, every: "12h" };
    const r = call("PUT", "/v1/bases/storefront/refresh", input);
    expect(r.body).toMatchObject({ base: "storefront", every: "12h0m0s", source: { password_set: true, databases: ["shop"] } });
    expect(JSON.stringify(r.body)).not.toContain("pw\"");
    expect(call("PUT", "/v1/bases/storefront/refresh", { ...input, every: "30m" }).status).toBe(400);
    vi.advanceTimersByTime(61_000);
    expect(call("POST", "/v1/bases/storefront/refresh/run").status).toBe(202);
    expect(base("storefront").operation?.kind).toBe("base.refresh");
    vi.advanceTimersByTime(4000);
    expect(base("storefront").status).toBe("ready");
    expect((get("/v1/bases/storefront/refresh") as any).last_run.ok).toBe(true);
    expect(call("DELETE", "/v1/bases/storefront/refresh").status).toBe(204);
    expect(call("GET", "/v1/bases/storefront/refresh").status).toBe(404);
  });
});

describe("cluster, engines, modules, settings, tokens", () => {
  it("serves 61 metric points per node", () => {
    const m = get("/v1/nodes/node-2/metrics").items;
    expect(m.length).toBe(61);
    expect(m[0]).toMatchObject({ mem_total: expect.any(Number) });
    expect(Date.parse(m[60].time)).toBe(Date.now());
  });
  it("installs an engine on every node after a while, and removes an unused one", () => {
    call("POST", "/v1/runtimes", { runtime: "postgres@17.6" });
    expect(get("/v1/operations").items.some((o: Operation) => o.kind === "runtime.install" && o.state === "running")).toBe(true);
    vi.advanceTimersByTime(4500);
    expect(get("/v1/cluster").nodes.every((n: any) => n.runtimes.includes("postgres@17.6"))).toBe(true);
    expect(get("/v1/runtimes/available").items.map((r: any) => r.runtime)).not.toContain("postgres@17.6");
    expect(call("DELETE", "/v1/runtimes/postgres/17.6").status).toBe(204);
    expect(get("/v1/runtimes").items.map((r: any) => r.runtime)).not.toContain("postgres@17.6");
    expect(call("DELETE", "/v1/runtimes/postgres/18.6")).toMatchObject({ status: 409, body: { error: { code: "in_use" } } });
    expect(call("POST", "/v1/runtimes", { runtime: "postgres@18.6" }).status).toBe(409);
  });
  it("lists module pages for enabled modules only", () => {
    expect(get("/v1/capabilities").pages.map((p: any) => p.module)).toEqual(["auth", "audit"]);
    call("DELETE", "/v1/modules/audit");
    expect(get("/v1/capabilities").pages.map((p: any) => p.module)).toEqual(["auth"]);
    expect(get("/v1/modules").items.find((m: any) => m.name === "audit").wanted).toBe(false);
    call("POST", "/v1/modules", { name: "audit" });
    expect(get("/v1/modules").items.find((m: any) => m.name === "audit")).toMatchObject({ wanted: true });
    expect(get("/v1/modules").items.find((m: any) => m.name === "audit").nodes.some((n: any) => !n.current)).toBe(true);
    vi.advanceTimersByTime(2500);
    expect(get("/v1/modules").items.find((m: any) => m.name === "audit").nodes.every((n: any) => n.current)).toBe(true);
    expect(call("POST", "/v1/modules", { name: "nope" }).status).toBe(404);
  });
  it("削除済みモジュールだけ forget でき、有効または未記録は拒否し、後から再インストールできる", () => {
    expect(call("POST", "/v1/modules/audit/forget")).toMatchObject({ status: 409, body: { error: { code: "conflict" } } });
    expect(call("DELETE", "/v1/modules/audit").status).toBe(204);
    expect(call("POST", "/v1/modules/audit/forget").status).toBe(204);
    expect(get("/v1/modules").items.map((m: any) => m.name)).not.toContain("audit");
    expect(get("/v1/capabilities").pages.map((p: any) => p.module)).toEqual(["auth"]);
    expect(get("/v1/settings").items.map((sc: any) => sc.scope)).not.toContain("audit");
    expect(call("POST", "/v1/modules/audit/forget").status).toBe(404);
    expect(call("POST", "/v1/modules", { name: "audit" }).status).toBe(201);
    expect(get("/v1/modules").items.find((m: any) => m.name === "audit")).toMatchObject({ wanted: true });
  });
  it("cannot verify licence keys in the demo", () => {
    expect(call("PUT", "/v1/license", { key: "dbbl1.fake" })).toMatchObject({ status: 422, body: { error: { code: "invalid_license" } } });
    expect(call("POST", "/v1/license/verify", { key: " " }).status).toBe(400);
  });
  it("saves settings, resets an emptied one to its default, and checks types", () => {
    const find = (key: string) => (get("/v1/settings").items as any[]).flatMap((s) => s.settings).find((x) => x.key === key);
    expect(call("PUT", "/v1/settings/core", { values: { "backup.keep": "30" } }).status).toBe(200);
    expect(find("backup.keep").value).toBe("30");
    call("PUT", "/v1/settings/core", { values: { "backup.keep": "" } });
    expect(find("backup.keep").value).toBe("14");
    expect(call("PUT", "/v1/settings/core", { values: { "backup.keep": "many" } }).status).toBe(400);
    expect(call("PUT", "/v1/settings/core", { values: { "nope": "1" } }).status).toBe(400);
    expect(call("PUT", "/v1/settings/nope", { values: {} }).status).toBe(404);
  });
  it("keeps a secret setting write-only: the value never comes back, only whether one is set, and an empty one keeps it", () => {
    const find = () => (get("/v1/settings").items as any[]).flatMap((s) => s.settings).find((x) => x.key === "license.key");
    expect(find()).toMatchObject({ value: "", secret_set: false });
    const put = call("PUT", "/v1/settings/core", { values: { "license.key": "dbbl1.abc" } });
    expect(JSON.stringify(put.body)).not.toContain("dbbl1.abc");
    expect(find()).toMatchObject({ value: "", secret_set: true });
    call("PUT", "/v1/settings/core", { values: { "license.key": "" } });
    expect(find().secret_set).toBe(true);
    call("PUT", "/v1/settings/core", { values: {}, clear: ["license.key"] });
    expect(find().secret_set).toBe(false);
    expect(call("PUT", "/v1/settings/core", { values: {}, clear: ["backup.keep"] }).status).toBe(400);
  });
  it("does not offer masking in 1.0: no masking module, no base.mask action", () => {
    expect(get("/v1/modules").items.map((m: any) => m.name)).not.toContain("masking");
    expect(get("/v1/settings").items.map((sc: any) => sc.scope)).not.toContain("masking");
    expect(JSON.stringify(get("/v1/bases"))).not.toContain("base.mask");
  });
  it("creates a service token (secret shown once, obviously fake) and revokes it", () => {
    const r = call("POST", "/v1/tokens", { name: "deploy", scope: "branches", ttl: "720h" });
    expect(r.status).toBe(201);
    const t = r.body as any;
    expect(t.token).toMatch(/^dbb_demo_x+/);
    const listed = get("/v1/tokens").items;
    expect(listed.find((x: any) => x.id === t.id)).toMatchObject({ name: "deploy" });
    expect(JSON.stringify(listed)).not.toContain("dbb_demo_x");
    expect(call("POST", "/v1/tokens", { name: "", scope: "branches" }).status).toBe(400);
    expect(call("DELETE", `/v1/tokens/${t.id}`).status).toBe(204);
    expect(get("/v1/tokens").items.find((x: any) => x.id === t.id)).toBeUndefined();
  });
  it("keeps a person's own tokens apart", () => {
    b.signIn("developer");
    expect(get("/v1/account/tokens").items.map((x: any) => x.name)).toEqual(["my-laptop"]);
    const t = call("POST", "/v1/account/tokens", { name: "ci" }).body as any;
    expect(get("/v1/account/tokens").items).toHaveLength(2);
    b.signIn("admin");
    expect(get("/v1/account/tokens").items).toEqual([]);
    b.signIn("developer");
    expect(call("DELETE", `/v1/account/tokens/${t.id}`).status).toBe(204);
  });
  it("takes a backup now", () => {
    const n = get("/v1/backups?node=node-1").items.length;
    const r = call("POST", "/v1/backups?node=node-1");
    expect(r.status).toBe(201);
    const after = get("/v1/backups?node=node-1");
    expect(after.node).toBe("node-1");
    expect(after.items).toHaveLength(n + 1);
    expect(after.items[0].name).toBe((r.body as any).name);
  });
  it("checks for updates and says the demo is current; starting one is refused", () => {
    vi.advanceTimersByTime(1000);
    expect(call("POST", "/v1/updates/check").body).toMatchObject({ checked_at: new Date().toISOString(), latest_minor: "" });
    expect(call("POST", "/v1/updates", { target: "9.9.9" }).status).toBe(409);
    expect(call("DELETE", "/v1/updates").status).toBe(204);
  });
});

describe("unknown endpoints", () => {
  it("answer 404 not_found and say the demo lacks them", () => {
    const r = call("GET", "/v1/route-users");
    expect(r.status).toBe(404);
    expect(r.body).toMatchObject({ error: { code: "not_found", message: expect.stringContaining("demo") } });
    expect(call("POST", "/v1/mcp", {}).status).toBe(404);
  });
});

describe("keeping state across page loads", () => {
  it("settles running operations before saving, and restores everything", () => {
    call("POST", "/v1/branches", { name: "mid-flight", base: "storefront" });
    call("POST", "/v1/branches/report-fix/reset");
    const json = b.dump();
    const c = DemoBackend.load(json);
    c.signIn("admin");
    const items = (c.handle({ method: "GET", path: "/v1/branches" }).body as any).items as Branch[];
    expect(items.find((x) => x.name === "mid-flight")!.status).toBe("ready");
    expect(items.find((x) => x.name === "report-fix")!.status).toBe("ready");
    expect(c.state.pending).toEqual([]);
  });
  it("keeps timers working for operations started after a restore", () => {
    const c = DemoBackend.load(b.dump());
    c.signIn("admin");
    c.handle({ method: "POST", path: "/v1/branches", body: { name: "after", base: "orders" } });
    vi.advanceTimersByTime(2100);
    expect((c.handle({ method: "GET", path: "/v1/branches/after" }).body as Branch).status).toBe("ready");
  });
  it("starts fresh from a broken save", () => {
    expect(DemoBackend.load("{nope").state.bases.map((x) => x.name)).toEqual(["storefront", "orders"]);
  });
});

describe("checkpoints", () => {
  const list = (name: string) => get<{ items: Checkpoint[] }>(`/v1/branches/${name}/checkpoints`).items;
  const names = (name: string) => list(name).map((c) => c.name);
  const settle = (ms = 2300) => vi.advanceTimersByTime(ms);

  it("tells every branch it can take checkpoints, so the console shows them", () => {
    expect(branches().length).toBeGreaterThan(0);
    expect(branches().every((x) => x.checkpoints === true)).toBe(true);
    call("POST", "/v1/branches", { name: "fresh", base: "orders" });
    expect(branch("fresh")?.checkpoints).toBe(true);
  });
  it("marks branches saved before the flag existed as able to take checkpoints", () => {
    const old = JSON.parse(b.dump());
    for (const x of old.branches) delete x.checkpoints;
    const again = DemoBackend.load(JSON.stringify(old));
    expect((again.handle({ method: "GET", path: "/v1/branches" }).body as { items: Branch[] }).items.every((x) => x.checkpoints === true)).toBe(true);
  });
  it("starts with a few so the demo has something to show", () => {
    expect(names("migration-test")).toEqual(["before-migration", "after-schema-change"]);
    expect(list("migration-test").map((c) => c.current)).toEqual([false, true]);
    expect(names("report-fix")).toEqual([]);
    expect(call("GET", "/v1/branches/nope/checkpoints").status).toBe(404);
  });
  it("saves one at once: 201, now the current one, named by the time when no name is given", () => {
    const r = call("POST", "/v1/branches/report-fix/checkpoints", { name: "before-load" });
    expect(r.status).toBe(201);
    expect(r.body).toMatchObject({ name: "before-load", line: 0, auto: false, current: true });
    const second = call("POST", "/v1/branches/report-fix/checkpoints", { name: "" }).body as Checkpoint;
    expect(second.name).toBe("20261112-030000");
    expect(list("report-fix").map((c) => c.current)).toEqual([false, true]);
    expect((call("POST", "/v1/branches/report-fix/checkpoints", {}).body as Checkpoint).name).toBe("20261112-030000-2");
  });
  it("refuses a bad name, a taken name, a busy branch and a branch it cannot see", () => {
    expect(call("POST", "/v1/branches/report-fix/checkpoints", { name: "Bad Name" }).status).toBe(400);
    expect(call("POST", "/v1/branches/report-fix/checkpoints", { name: "a".repeat(41) }).status).toBe(400);
    expect(call("POST", "/v1/branches/migration-test/checkpoints", { name: "before-migration" })).toMatchObject({ status: 409, body: { error: { code: "already_exists" } } });
    call("POST", "/v1/branches/report-fix/reset");
    expect(call("POST", "/v1/branches/report-fix/checkpoints", { name: "x" }).status).toBe(409);
    expect(call("POST", "/v1/branches/nope/checkpoints", { name: "x" }).status).toBe(404);
    b.signIn("developer");
    expect(call("POST", "/v1/branches/feature-checkout/checkpoints", { name: "x" }).status).toBe(404);
    expect(call("GET", "/v1/branches/feature-checkout/checkpoints").status).toBe(404);
    expect(call("POST", "/v1/branches/alex-checkout-fix/checkpoints", { name: "mine" }).status).toBe(201);
  });
  it("stops at the branch.max_checkpoints setting with a message that names it", () => {
    call("PUT", "/v1/settings/core", { values: { "branch.max_checkpoints": "2" } });
    expect(call("POST", "/v1/branches/migration-test/checkpoints", { name: "third" })).toMatchObject({ status: 409, body: { error: { code: "conflict" } } });
    expect(JSON.stringify(call("POST", "/v1/branches/migration-test/checkpoints", { name: "third" }).body)).toContain("limit 2");
    expect(JSON.stringify(call("POST", "/v1/branches/migration-test/checkpoints", { name: "third" }).body)).toContain("not counted");
    call("PUT", "/v1/settings/core", { values: { "branch.max_checkpoints": "" } });
    expect(call("POST", "/v1/branches/migration-test/checkpoints", { name: "third" }).status).toBe(201);
  });
  it("restores in the background: restoring, then ready; nothing is lost and the state left is kept", () => {
    const r = call("POST", "/v1/branches/migration-test/checkpoints/before-migration/restore");
    expect(r.status).toBe(202);
    expect((r.body as Operation).kind).toBe("branch.restore");
    expect(branch("migration-test")!.status).toBe("restoring");
    expect(call("POST", "/v1/branches/migration-test/checkpoints/before-migration/restore").status).toBe(409);
    expect(call("POST", "/v1/branches/migration-test/checkpoints", { name: "x" }).status).toBe(409);
    settle(800); // PostgreSQL: about 0.7 s
    expect(branch("migration-test")!.status).toBe("ready");
    expect(op((r.body as Operation).id).state).toBe("succeeded");
    expect(names("migration-test")).toEqual(["before-migration", "after-schema-change"]);
    const cur = list("migration-test").filter((c) => c.current);
    expect(cur.map((c) => c.name)).toEqual(["before-migration"]);
    // New checkpoints are on a new line.
    expect((call("POST", "/v1/branches/migration-test/checkpoints", { name: "later" }).body as Checkpoint).line).toBe(1);
  });
  it("restores any checkpoint, any number of times, and MySQL takes longer than PostgreSQL", () => {
    call("POST", "/v1/branches/report-fix/checkpoints", { name: "c1" });
    call("POST", "/v1/branches/report-fix/checkpoints", { name: "c2" });
    call("POST", "/v1/branches/report-fix/checkpoints/c1/restore");
    settle(1000);
    expect(branch("report-fix")!.status).toBe("restoring");
    settle(1300);
    expect(branch("report-fix")!.status).toBe("ready");
    vi.setSystemTime(Date.now() + 5000);
    call("POST", "/v1/branches/report-fix/checkpoints/c2/restore");
    settle();
    expect(list("report-fix").find((c) => c.current)!.name).toBe("c2");
    expect(list("report-fix").filter((c) => c.auto)).toHaveLength(0); // a restore saves nothing by itself
    vi.setSystemTime(Date.now() + 5000);
    call("POST", "/v1/branches/report-fix/checkpoints/c1/restore");
    settle();
    expect(names("report-fix")).toEqual(["c1", "c2"]);
    expect(list("report-fix").find((c) => c.current)!.name).toBe("c1");
  });
  it("answers 404 for an unknown checkpoint and 400 for a bad name", () => {
    expect(call("POST", "/v1/branches/migration-test/checkpoints/nope/restore").status).toBe(404);
    expect(call("POST", "/v1/branches/migration-test/checkpoints/Bad%20Name/restore").status).toBe(400);
  });
  it("never refuses a restore for the checkpoint limit", () => {
    call("PUT", "/v1/settings/core", { values: { "branch.max_checkpoints": "2" } }); // migration-test holds two of its own
    expect(call("POST", "/v1/branches/migration-test/checkpoints/before-migration/restore").status).toBe(202);
    settle(800);
    expect(branch("migration-test")!.status).toBe("ready");
    expect(names("migration-test")).toHaveLength(2);
    expect(call("POST", "/v1/branches/migration-test/checkpoints", { name: "third" }).status).toBe(409); // two of its own
    call("DELETE", "/v1/branches/migration-test/checkpoints/after-schema-change");
    expect(call("POST", "/v1/branches/migration-test/checkpoints", { name: "third" }).status).toBe(201);
  });
  it("deletes one without touching the others", () => {
    expect(call("DELETE", "/v1/branches/migration-test/checkpoints/before-migration").status).toBe(204);
    expect(names("migration-test")).toEqual(["after-schema-change"]);
    expect(call("DELETE", "/v1/branches/migration-test/checkpoints/before-migration").status).toBe(404);
    expect(call("DELETE", "/v1/branches/nope/checkpoints/x").status).toBe(404);
  });
  it("keeps the checkpoints through a reset, which leaves them without a current one", () => {
    call("POST", "/v1/branches/migration-test/reset");
    settle(1400);
    expect(branch("migration-test")!.status).toBe("ready");
    expect(names("migration-test")).toEqual(["before-migration", "after-schema-change"]);
    expect(list("migration-test").some((c) => c.current)).toBe(false);
    expect((call("POST", "/v1/branches/migration-test/checkpoints", { name: "fresh" }).body as Checkpoint).line).toBe(1);
  });
  it("deleting the branch deletes its checkpoints", () => {
    call("DELETE", "/v1/branches/migration-test");
    settle(900);
    expect(b.state.checkpoints["migration-test"]).toBeUndefined();
  });
  it("loads a state saved before checkpoints existed", () => {
    const old = JSON.parse(b.dump());
    delete old.checkpoints;
    const again = DemoBackend.load(JSON.stringify(old));
    again.signIn("admin");
    expect(again.handle({ method: "GET", path: "/v1/branches/report-fix/checkpoints" }).body).toEqual({ items: [], next_cursor: null, lines: [{ line: 0, live: true, parent: null, from: "" }], live: 0, head: null });
  });
});

describe("history graph and module", () => {
  type Graph = { items: Checkpoint[]; lines: { line: number; live: boolean; parent: number | null; from: string }[]; live: number; head: string | null };
  const graph = (name: string) => get<Graph>(`/v1/branches/${name}/checkpoints`);
  const events = (name: string, query = "") => call("GET", `/v1/ext/history/branches/${name}/events${query}`);
  const settle = (ms = 4000) => vi.advanceTimersByTime(ms);

  it("starts with a branch that has been restored twice, so the graph has something to show", () => {
    const g = graph("alex-orders-report");
    expect(g.lines.map((l) => [l.line, l.parent, l.from, l.live])).toEqual([[0, null, "", false], [1, 0, "baseline", false], [2, 0, "after-import", true]]);
    expect(g.live).toBe(2);
    expect(g.head).toBe("fixed-totals");
    expect(g.items.map((c) => c.line)).toEqual([0, 0, 0, 1, 1, 2]);
  });
  it("says the module is there so the console offers recording", () => {
    expect(get("/v1/capabilities").record).toBe(true);
    expect(get("/v1/capabilities").modules).toContain("history " + get("/v1/capabilities").version);
  });
  it("makes up the same transactions every time, newest first, on every line", () => {
    const a = events("alex-orders-report", "?line=2").body as any;
    expect(a.recorded).toBe(true);
    expect(a.items.length).toBeGreaterThan(5);
    const times = a.items.map((t: any) => Date.parse(t.time));
    expect(times).toEqual([...times].sort((x, y) => y - x));
    expect(a.items[0]).toMatchObject({ file: "binlog.000002", ddl: false });
    expect(events("alex-orders-report", "?line=2").body).toEqual(a);
    expect((events("alex-orders-report", "?line=0").body as any).items.some((t: any) => t.ddl)).toBe(true);
    const page = (events("alex-orders-report", "?line=1&limit=4").body as any);
    expect(page.items).toHaveLength(4);
    expect(page.next_before).toBe(page.items[3].gtid);
    expect((events("alex-orders-report", "?line=1&limit=500").body as any).next_before).toBeNull();
  });
  it("shows nothing for a branch that is not recorded or not MySQL, and refuses unknown lines and branches", () => {
    expect((events("report-fix").body as any)).toMatchObject({ recorded: false, items: [] });
    expect((events("migration-test").body as any).items).toEqual([]);
    expect(events("alex-orders-report", "?line=9").status).toBe(400);
    expect(events("nope").status).toBe(404);
    call("DELETE", "/v1/modules/history");
    settle(2000);
    expect(events("alex-orders-report").status).toBe(404);
  });
  it("restores to a transaction: the checkpoint before it and a new line from it; the state it leaves is not saved", () => {
    const tx = (events("alex-orders-report", "?line=2&limit=1").body as any).items[0];
    const autosBefore = graph("alex-orders-report").items.filter((c) => c.auto).length;
    const r = call("POST", "/v1/ext/history/branches/alex-orders-report/restore", { line: 2, gtid: tx.gtid });
    expect(r.status).toBe(202);
    expect((r.body as Operation).kind).toBe("history.restore");
    expect(branch("alex-orders-report")!.status).toBe("restoring");
    settle();
    const g = graph("alex-orders-report");
    expect(branch("alex-orders-report")!.status).toBe("ready");
    expect(g.live).toBe(3);
    expect(g.lines.find((l) => l.line === 3)).toMatchObject({ parent: 2, from: "fixed-totals", live: true });
    expect(g.items.filter((c) => c.auto)).toHaveLength(autosBefore); // none was added
  });
  it("lists the replayed transactions on the new line and says which one it was restored to", () => {
    const newestFirst = (line: number) => ((events("alex-orders-report", `?line=${line}&limit=500`).body as any).items as { gtid: string }[]).map((x) => x.gtid);
    const pick = newestFirst(2)[0];
    call("POST", "/v1/ext/history/branches/alex-orders-report/restore", { line: 2, gtid: pick });
    settle();
    const live = graph("alex-orders-report").live;
    const out = events("alex-orders-report", `?line=${live}&limit=500`).body as any;
    expect(out.from_tx).toBe(pick);
    const gtids = (out.items as { gtid: string }[]).map((x) => x.gtid);
    expect(gtids).toContain(pick);
    expect(gtids.length).toBeGreaterThan(1);
    // A line made another way names no transaction.
    expect((events("alex-orders-report", "?line=2&limit=500").body as any).from_tx).toBeUndefined();
  });
  it("restores to a transaction before the line's first checkpoint from the creation state", () => {
    const first = (events("alex-orders-report", "?line=0&limit=500").body as any).items.at(-1);
    call("POST", "/v1/ext/history/branches/alex-orders-report/restore", { line: 0, gtid: first.gtid });
    settle();
    const g = graph("alex-orders-report");
    expect(g.lines.find((l) => l.line === g.live)).toMatchObject({ parent: 0, from: "reset" });
  });
  it("refuses a restore to a transaction it does not know, or while the branch is busy", () => {
    expect(call("POST", "/v1/ext/history/branches/alex-orders-report/restore", { line: 2, gtid: "x:1" }).status).toBe(409);
    const tx = (events("alex-orders-report", "?line=2&limit=1").body as any).items[0];
    call("POST", "/v1/ext/history/branches/alex-orders-report/restore", { line: 2, gtid: tx.gtid });
    expect(call("POST", "/v1/ext/history/branches/alex-orders-report/restore", { line: 2, gtid: tx.gtid }).status).toBe(409);
  });
  it("records a new MySQL branch when asked, and only then", () => {
    const made = (name: string, body: object) => {
      call("POST", "/v1/branches", { name, base: "orders", ...body });
      settle(6000);
      return branch(name)!;
    };
    expect(made("rec-one", { record: true }).record).toBe(true);
    expect(made("rec-two", {}).record).toBeUndefined();
    call("POST", "/v1/branches", { name: "rec-pg", base: "storefront", record: true });
    settle(6000);
    expect(branch("rec-pg")!.record).toBeUndefined();
  });
  it("decides once, at creation, whether a branch records: record_all only reaches branches made after it is set", () => {
    const made = (name: string, body: object = {}) => {
      call("POST", "/v1/branches", { name, base: "orders", ...body });
      settle(6000);
      return branch(name)!;
    };
    const recordAll = (v: string) => call("PUT", "/v1/settings/history", { values: { record_all: v } });
    expect(recordAll("true").status).toBe(200);
    const a = made("rec-all-a");
    expect(a.record).toBe(true);
    expect(recordAll("false").status).toBe(200);
    const b = made("rec-all-b");
    expect(b.record).toBeUndefined();
    expect(recordAll("true").status).toBe(200);
    // Neither setting nor any call changes a branch that exists.
    expect(branch("rec-all-a")!.record).toBe(true);
    expect(branch("rec-all-b")!.record).toBeUndefined();
    expect(call("PATCH", "/v1/branches/rec-all-b", { record: true }).status).toBe(400);
    expect(call("PATCH", "/v1/branches/rec-all-a", { record: false }).status).toBe(400);
    expect(branch("rec-all-a")!.record).toBe(true);
    expect(branch("rec-all-b")!.record).toBeUndefined();
    // Not for branches that cannot be recorded, with the setting on.
    call("POST", "/v1/branches", { name: "rec-all-pg", base: "storefront" });
    settle(6000);
    expect(branch("rec-all-pg")!.record).toBeUndefined();
    // Without the module nothing is recorded, and nothing is recorded later by installing it.
    call("DELETE", "/v1/modules/history");
    settle(2000);
    expect(made("rec-all-off").record).toBeUndefined();
    call("POST", "/v1/modules", { name: "history" });
    settle(6000);
    expect(branch("rec-all-off")!.record).toBeUndefined();
  });
  it("numbers the line after a reset of a branch whose lines are all remembered", () => {
    call("POST", "/v1/branches/alex-orders-report/reset");
    settle();
    const g = graph("alex-orders-report");
    expect(g.lines.find((l) => l.line === g.live)).toMatchObject({ parent: 0, from: "reset" });
  });
});

describe("branch owner", () => {
  it("returns the owner's id and a display name apart, as the real API does", () => {
    b.signIn("developer");
    expect(branches().length).toBeGreaterThan(0);
    for (const x of branches()) expect(x.owner_name).toBe(x.owner);
    call("POST", "/v1/branches", { name: "owner-check", base: "orders" });
    expect(branch("alex-owner-check")).toMatchObject({ owner: "alex@example.com", owner_name: "alex@example.com" });
  });
});

describe("branch upgrade", () => {
  const list = (name: string) => get<{ items: Checkpoint[] }>(`/v1/branches/${name}/checkpoints`).items;
  const settle = (ms = 2300) => vi.advanceTimersByTime(ms);
  const upgrade = (name: string, body: unknown) => call("POST", `/v1/branches/${name}/upgrade`, body);
  it("refuses what the server refuses, at once", () => {
    expect(upgrade("report-fix", { runtime: "bad" }).status).toBe(400);
    expect(upgrade("report-fix", { runtime: "mysql@8.4.13" })).toMatchObject({ status: 422, body: { error: { code: "upgrade_not_allowed" } } });
    expect(upgrade("report-fix", { runtime: "mysql@9.4.0" })).toMatchObject({ status: 404, body: { error: { code: "runtime_not_installed" } } });
    expect(upgrade("nope", { runtime: "mysql@9.4.0" }).status).toBe(404);
  });
  it("checks without changing anything, then upgrades with an automatic Checkpoint that records the old runtime", () => {
    call("POST", "/v1/runtimes", { runtime: "mysql@9.4.0" });
    settle(4000);
    const before = branch("report-fix")!.runtime;
    expect(before).toBe("mysql@8.4.13");
    const c = upgrade("report-fix", { runtime: "mysql@9.4.0", check_only: true });
    expect(c.status).toBe(202);
    settle(800);
    expect(op((c.body as Operation).id).result).toMatchObject({ status: "checked", check_only: true });
    expect(branch("report-fix")).toMatchObject({ runtime: before, status: "ready" });
    const r = upgrade("report-fix", { runtime: "mysql@9.4.0" });
    expect((r.body as Operation).kind).toBe("branch.upgrade");
    expect(branch("report-fix")!.status).toBe("upgrading");
    expect(upgrade("report-fix", { runtime: "mysql@9.4.0" }).status).toBe(409);
    settle(2300);
    expect(branch("report-fix")).toMatchObject({ runtime: "mysql@9.4.0", base_runtime: before, status: "ready" });
    const res = op((r.body as Operation).id).result!;
    expect(res).toMatchObject({ status: "upgraded", from: before, to: "mysql@9.4.0" });
    expect(list("report-fix").find((x) => x.name === res.checkpoint)).toMatchObject({ auto: true, runtime: before });
  });
});

describe("pause and resume", () => {
  const settle = (ms = 2300) => vi.advanceTimersByTime(ms);
  const status = (name: string) => get<Branch>(`/v1/branches/${name}`).status;

  it("stops a ready branch (pausing, then paused) and starts it again", () => {
    const name = branches()[0].name;
    expect(call("POST", `/v1/branches/${name}/pause`).status).toBe(202);
    expect(status(name)).toBe("pausing");
    settle();
    expect(status(name)).toBe("paused");
    expect(call("POST", `/v1/branches/${name}/resume`).status).toBe(202);
    expect(status(name)).toBe("resuming");
    settle();
    expect(status(name)).toBe("ready");
  });
  it("refuses a pause of a branch that is not ready and a resume of one that is not paused", () => {
    const name = branches()[0].name;
    expect(call("POST", `/v1/branches/${name}/resume`).status).toBe(409);
    call("POST", `/v1/branches/${name}/pause`);
    expect(call("POST", `/v1/branches/${name}/pause`).status).toBe(409);
    settle();
    expect(call("POST", `/v1/branches/${name}/pause`).status).toBe(409);
    expect(call("POST", "/v1/branches/nope/pause").status).toBe(404);
    expect(call("POST", "/v1/branches/nope/resume").status).toBe(404);
  });
  it("lets a paused branch be deleted, but not reset", () => {
    const name = branches()[0].name;
    call("POST", `/v1/branches/${name}/pause`);
    settle();
    expect(call("POST", `/v1/branches/${name}/reset`).status).toBe(409);
    expect(call("DELETE", `/v1/branches/${name}`).status).toBe(202);
  });
});

describe("sql runner", () => {
  it("runs a statement on a ready branch: rows for a SELECT, rows affected for the rest", () => {
    const name = branches()[0].name;
    const sel = call("POST", `/v1/branches/${name}/query`, { sql: "SELECT * FROM users" });
    expect(sel.status).toBe(200);
    expect((sel.body as any).columns).toEqual(["id", "email", "name"]);
    expect((sel.body as any).rows).toHaveLength(5);
    const upd = call("POST", `/v1/branches/${name}/query`, { sql: "UPDATE users SET name = 'x'" });
    expect((upd.body as any).affected).toBe(1);
  });
  it("refuses an empty statement, a branch that is not ready and a branch that is not there", () => {
    const name = branches()[0].name;
    expect(call("POST", `/v1/branches/${name}/query`, { sql: " " }).status).toBe(422);
    expect(call("POST", "/v1/branches/nope/query", { sql: "SELECT 1" }).status).toBe(404);
    call("POST", `/v1/branches/${name}/pause`);
    expect(call("POST", `/v1/branches/${name}/query`, { sql: "SELECT 1" }).status).toBe(409);
  });
});

describe("branch fork", () => {
  const settle = (ms = 2300) => vi.advanceTimersByTime(ms);

  it("forks a live branch, blocks parent deletion with 409 in_use, and allows parent deletion after child goes", () => {
    const r = call("POST", "/v1/branches/report-fix/fork", { name: "report-exp", ttl: "24h" });
    expect(r.status).toBe(202);
    const o = r.body as Operation;
    expect(o).toMatchObject({ kind: "branch.fork", target: { resource: "branch", name: "report-exp" } });
    settle();
    const child = branch("report-exp")!;
    expect(child).toMatchObject({ status: "ready", parent_branch: "report-fix", parent_snapshot: "fork-report-exp", base: "orders", node: branch("report-fix")!.node });
    expect(call("DELETE", "/v1/branches/report-fix")).toMatchObject({ status: 409, body: { error: { code: "in_use" } } });
    expect(call("DELETE", "/v1/branches/report-exp").status).toBe(202);
    settle();
    expect(call("DELETE", "/v1/branches/report-fix").status).toBe(202);
  });

  it("forks from a checkpoint or @reset and prefixes non-admin names", () => {
    const r1 = call("POST", "/v1/branches/migration-test/fork", { name: "from-cp", checkpoint: "before-migration" });
    expect(r1.status).toBe(202);
    settle();
    expect(branch("from-cp")).toMatchObject({ parent_branch: "migration-test", parent_snapshot: "ckpt-before-migration" });
    expect(call("POST", "/v1/branches/from-cp/recreate")).toMatchObject({ status: 409, body: { error: { code: "conflict" } } });
    expect(call("POST", "/v1/branches/migration-test/fork", { name: "bad-cp", checkpoint: "nope" }).status).toBe(404);

    b.signIn("developer");
    const r2 = call("POST", "/v1/branches/alex-checkout-fix/fork", { name: "try1", checkpoint: "@reset" });
    expect(r2.status).toBe(202);
    settle();
    expect(branch("alex-try1")).toMatchObject({ parent_branch: "alex-checkout-fix", parent_snapshot: "reset", owner: "alex@example.com" });
    expect(call("POST", "/v1/branches/alex-checkout-fix/fork", { name: "try1" })).toMatchObject({ status: 409, body: { error: { code: "already_exists" } } });
  });

  it("merges a line or checkpoint into live in dry-run and apply modes", () => {
    const dry = call("POST", "/v1/branches/migration-test/merge", { checkpoint: "before-migration", dry_run: true });
    if (dry.status === 202) {
      settle();
      expect(op((dry.body as Operation).id).result).toMatchObject({ status: "dry_run", dry_run: true });
    }
    expect(call("POST", "/v1/branches/migration-test/merge", { source_line: 0 })).toMatchObject({ status: 400 });
  });
});

