// A pretend db-branch agent: the /v1 API the console calls, answered from an
// in-memory state, with the same shapes, status codes and error codes as the
// Go handlers (internal/interface/httpapi). Operations take realistic time and
// publish the same events as the real server. No network, no real data.
import type { APIToken, Base, BaseRefresh, Branch, Checkpoint, HistoryTx, Operation, SettingScope, Upload } from "../api";
import { ownerPrefix } from "../lib/branchname";
import { upgradeProblem, type Finding, type UpgradeResult } from "../lib/upgrade";
import {
  ADMIN,
  BaseRec,
  DEMO_HOST,
  DEMO_PASSWORD,
  DEVELOPER,
  Effect,
  GIB,
  MIB,
  MODULES,
  newId,
  NODES,
  Pending,
  PROTOCOL,
  CheckpointState,
  LineMeta,
  runtimeEngine,
  seed,
  snapshotName,
  State,
  VERSION,
  Who,
} from "./state";

export type Request = { method: string; path: string; body?: unknown };
export type Reply = { status: number; body?: unknown };
export type Listener = (type: string, data: unknown) => void;

// How long each simulated operation takes (ms).
// A restore stops the server and starts it again: PostgreSQL stops faster than MySQL (measured on real ZFS: 0.7 s and 2.2 s).
export const TIMING = { branchCreate: 2000, branchReset: 1300, branchDelete: 800, restorePostgres: 700, restoreMysql: 2200, baseCreate: 2500, baseSnapshot: 1500, baseDelete: 1200, refresh: 3500, runtime: 4000, module: 2000 };

const MAX_NAME = 32;
const NAME = /^[a-z]([a-z0-9]|-[a-z0-9])*$/;
const ANON = { max_ttl: "4h0m0s", max_branches: 2 };
const ANON_TTL = 4 * 3600e3;
const DAY = 24 * 3600e3;
const CHECKPOINT_NAME = /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/;
const MAX_CHECKPOINT_NAME = 40;
const CONSOLE_ACTIONS = ["base.read", "base.create", "base.delete", "base.load", "base.snapshot", "base.publish", "base.refresh", "base.refresh.read", "runtime.install", "cluster.read", "setting.write", "module.install", "token.write", "backup.read", "route-user.write", "update.read", "update.check", "update.apply", "branch.checkpoint", "branch.checkpoint_delete", "branch.restore", "branch.upgrade"];
const BASE_ACTIONS = ["base.snapshot", "base.load", "base.delete", "base.describe", "base.mask", "base.publish", "base.sql", "base.start", "base.stop", "base.refresh", "base.refresh.read", "base.snapshot_delete", "base.replicas"];

/** Go-style durations: "4h", "90m", "1h30m0s". Null when it is not one. */
export function parseDuration(s: string): number | null {
  if (!/^(\d+(\.\d+)?[hms])+$/.test(s)) return null;
  let ms = 0;
  for (const m of s.matchAll(/(\d+(?:\.\d+)?)([hms])/g)) ms += Number(m[1]) * { h: 3600e3, m: 60e3, s: 1e3 }[m[2] as "h" | "m" | "s"];
  return ms;
}

/** goDuration writes ms the way Go prints a time.Duration: "12h0m0s", "30m0s". */
export function goDuration(ms: number): string {
  const s = Math.round(ms / 1000);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  return `${h ? h + "h" : ""}${m}m${s % 60}s`.replace(/^(\d+h)(\d+m)/, "$1$2");
}

type Access = "public" | "anon" | "user" | "admin";
type Ctx = { p: Record<string, string>; q: URLSearchParams; body: any; who: Who | null };
type Route = { method: string; re: RegExp; access: Access; run: (c: Ctx) => Reply };

const ok = (body?: unknown, status = 200): Reply => ({ status, body });
const fail = (status: number, code: string, message: string): Reply => ({ status, body: { error: { code, message } } });
// Automatic checkpoints (taken before an upgrade) kept per branch (domain.MaxAutoCheckpoints).
const MAX_AUTO_CHECKPOINTS = 5;

const notFound = (what: string) => fail(404, "not_found", `${what} not found`);

export class DemoBackend {
  state: State;
  private listeners = new Set<Listener>();
  private routes: Route[] = [];

  constructor(state?: State) {
    this.state = state ?? seed(Date.now());
    this.state.checkpoints ??= {}; // saved by a version without checkpoints
    for (const br of this.state.branches) br.checkpoints ??= true; // likewise: the demo always can
    this.build();
  }

  /** load restores a state saved by dump(); anything unreadable starts afresh. */
  static load(json: string | null): DemoBackend {
    try {
      const s = JSON.parse(json ?? "") as State;
      if (!Array.isArray(s.bases) || !Array.isArray(s.branches)) throw new Error("not a state");
      return new DemoBackend(s);
    } catch {
      return new DemoBackend();
    }
  }

  /** dump finishes every running operation, then serialises the state. */
  dump(): string {
    for (const p of [...this.state.pending]) this.settle(p);
    return JSON.stringify(this.state);
  }

  on(fn: Listener): () => void {
    this.listeners.add(fn);
    return () => void this.listeners.delete(fn);
  }

  signIn(who: "admin" | "developer" | Who) {
    this.state.user = who === "admin" ? ADMIN : who === "developer" ? DEVELOPER : who;
    this.emit("resync", {});
  }

  signOut() {
    this.state.user = null;
  }

  private emit(type: string, data: unknown) {
    for (const l of [...this.listeners]) l(type, data);
  }

  handle(req: Request): Reply {
    const url = new URL(req.path, "http://demo.invalid");
    const method = req.method.toUpperCase();
    const who = this.state.user;
    let seen = false;
    for (const r of this.routes) {
      const m = url.pathname.match(r.re);
      if (!m) continue;
      seen = true;
      if (r.method !== method) continue;
      if (!who && r.access !== "public" && !(r.access === "anon")) return fail(401, "unauthenticated", "log in first");
      if (r.access === "admin" && !who!.admin) return fail(403, "forbidden", "your role cannot do this");
      const body = req.body && typeof req.body === "object" ? req.body : {};
      return r.run({ p: decodeParams(m.groups ?? {}), q: url.searchParams, body, who });
    }
    return seen ? fail(405, "invalid_request", `${method} is not allowed here`) : fail(404, "not_found", `the demo does not implement ${method} ${url.pathname}`);
  }

  // ---- plumbing ----

  private newOp(kind: string, resource: string, name: string): Operation {
    const op: Operation = { id: "op-" + newId(this.state).slice(0, 8), kind, target: { resource, name }, state: "running" };
    this.state.ops.push(op);
    this.emit("operation.updated", op);
    return op;
  }

  private schedule(op: Operation | null, ms: number, fx: Effect) {
    const p: Pending = { op: op?.id ?? "", due: Date.now() + ms, fx };
    this.state.pending.push(p);
    setTimeout(() => this.settle(p), ms);
  }

  private settle(p: Pending) {
    const i = this.state.pending.indexOf(p);
    if (i < 0) return;
    this.state.pending.splice(i, 1);
    this.apply(p.fx);
    const op = this.state.ops.find((o) => o.id === p.op);
    if (op) {
      op.state = "succeeded";
      this.emit("operation.updated", op);
    }
  }

  private apply(fx: Effect) {
    const s = this.state;
    const now = Date.now();
    switch (fx.k) {
      case "branch.ready": {
        const b = s.branches.find((x) => x.name === fx.name);
        if (!b) return;
        b.status = "ready";
        b.operation = null;
        b.storage = { used: 300 * 1024, referenced: 330 * 1024 };
        if (fx.reset) {
          b.reset_at = new Date(now).toISOString();
          this.resetCheckpoints(b.name);
        }
        this.emit("branch.updated", { name: b.name });
        return;
      }
      case "branch.restore": {
        const b = s.branches.find((x) => x.name === fx.name);
        if (!b) return;
        this.finishRestore(b, fx.checkpoint, fx.replay);
        this.emit("branch.updated", { name: b.name });
        return;
      }
      case "op.result": {
        const op = s.ops.find((o) => o.id === fx.op);
        if (op) op.result = fx.result;
        return;
      }
      case "branch.upgrade": {
        const b = s.branches.find((x) => x.name === fx.name);
        const op = s.ops.find((o) => o.id === fx.op);
        if (op) op.result = fx.result;
        if (!b) return;
        this.finishUpgrade(b, fx.to, fx.result.checkpoint ?? "", now);
        this.emit("branch.updated", { name: b.name });
        return;
      }
      case "branch.delete":
        s.branches = s.branches.filter((x) => x.name !== fx.name);
        delete s.checkpoints[fx.name];
        this.emit("branch.deleted", { name: fx.name });
        return;
      case "base.ready": {
        const b = this.base(fx.name);
        if (!b) return;
        b.status = "ready";
        b.operation = null;
        if (fx.grow) b.size += fx.grow;
        if (fx.snapshot) this.takeSnapshot(b);
        this.emit("base.updated", { name: b.name });
        return;
      }
      case "base.delete":
        s.bases = s.bases.filter((x) => x.name !== fx.name);
        this.emit("base.deleted", { name: fx.name });
        return;
      case "refresh.run": {
        const b = this.base(fx.name);
        if (!b) return;
        b.status = "ready";
        b.operation = null;
        this.takeSnapshot(b);
        if (b.refresh) {
          const started = new Date(now - TIMING.refresh).toISOString();
          b.refresh.last_run = { started_at: started, finished_at: new Date(now).toISOString(), ok: true, error: null };
          b.refresh.next_run = new Date(now + (parseDuration(b.refresh.every) ?? DAY)).toISOString();
          b.refresh.operation = null;
        }
        this.emit("base.updated", { name: b.name });
        return;
      }
      case "runtime.install":
        if (!s.installed.includes(fx.runtime)) s.installed = [...s.installed, fx.runtime].sort();
        s.available = s.available.filter((r) => r.runtime !== fx.runtime);
        this.emit("runtime.updated", { runtime: fx.runtime });
        return;
      case "module.ready":
        if (s.modules[fx.name] === "installing") s.modules[fx.name] = "on";
        this.emit("resync", {});
    }
  }

  private base(name: string): BaseRec | undefined {
    return this.state.bases.find((b) => b.name === name);
  }

  private takeSnapshot(b: BaseRec) {
    let at = Date.now();
    while (b.snapshots.some((x) => x.name === snapshotName(at))) at += 1000;
    b.snapshots.push({ name: snapshotName(at), created_at: new Date(at).toISOString(), public: false });
    b.latest_snapshot = snapshotName(at);
  }

  // ---- views ----

  private baseView(b: BaseRec, who: Who | null): Base {
    const held = (n: string) => n === b.node || b.replica_nodes === null || (b.replica_nodes ?? []).includes(n);
    const others = NODES.filter((n) => n !== b.node && held(n));
    const { size: _size, refresh: _refresh, ...rest } = b;
    return {
      ...rest,
      replicas: Object.fromEntries(b.snapshots.map((s) => [s.name, others])),
      holders: NODES.filter(held).map((node) => ({ node, state: "held" as const, origin: node === b.node })),
      can: Object.fromEntries(BASE_ACTIONS.map((a) => [a, !!who?.admin])),
    };
  }

  private session(who: Who) {
    const can: Record<string, boolean> = {};
    for (const a of CONSOLE_ACTIONS) can[a] = who.admin || a === "base.read" || a.startsWith("branch."); // people manage their own branches
    for (const m of Object.keys(MODULES)) can[`ext.${m}.read`] = can[`ext.${m}.write`] = who.admin;
    return {
      user: { id: who.id, name: who.name, roles: who.roles },
      csrf_token: this.state.csrf,
      expires_at: new Date(Date.now() + 12 * 3600e3).toISOString(),
      can_change_password: true,
      language: this.state.language,
      can,
      branch_prefix: who.admin ? "" : prefixOf(who),
    };
  }

  private visible(who: Who | null) {
    return this.state.branches.filter((b) => (who ? who.admin || b.owner === who.id : b.owner === null));
  }

  private branchFor(c: Ctx): Branch | undefined {
    return this.visible(c.who).find((b) => b.name === c.p.name);
  }

  private moduleView(name: string) {
    const st = this.state.modules[name];
    return {
      name,
      version: VERSION,
      wanted: st !== "off",
      requires_license: MODULES[name].paid,
      nodes: NODES.map((node) => (st === "installing" ? { node, loaded: "", current: false } : { node, loaded: VERSION, current: true })),
    };
  }

  private scopes(): SettingScope[] {
    return this.state.settings.filter((s) => s.scope === "core" || this.state.modules[s.scope] !== "off");
  }

  // ---- routes ----

  private add(method: string, pattern: string, access: Access, run: (c: Ctx) => Reply) {
    const re = new RegExp("^" + pattern.replace(/:(\w+)/g, "(?<$1>[^/]+)").replace(/\*(\w+)/, "(?<$1>.+)") + "/?$");
    this.routes.push({ method, re, access, run });
  }

  private build() {
    const s = () => this.state;
    const a = this.add.bind(this);

    // Signing in: the demo takes any credentials.
    a("POST", "/v1/auth/login", "public", (c) => {
      const name = String(c.body.username ?? "").trim() || "admin";
      const id = name.toLowerCase();
      this.signIn(/^(demo-)?admin$/.test(id) ? ADMIN : id === DEVELOPER.id ? DEVELOPER : { id, name: id, roles: ["developer"], admin: false });
      return ok(this.session(s().user!));
    });
    a("GET", "/v1/auth/methods", "public", () => ok({ items: [] }));
    a("GET", "/v1/auth/session", "user", (c) => ok(this.session(c.who!)));
    a("POST", "/v1/auth/logout", "user", () => (this.signOut(), ok(undefined, 204)));
    a("PUT", "/v1/auth/password", "user", (c) => (String(c.body.new_password ?? "").length < 12 ? fail(400, "weak_password", "use at least 12 characters") : ok(undefined, 204)));
    a("GET", "/v1/account/preferences", "user", () => ok({ language: s().language, languages: ["en", "ja"] }));
    a("PUT", "/v1/account/preferences", "user", (c) => {
      const l = String(c.body.language ?? "");
      if (!["", "en", "ja"].includes(l)) return fail(400, "invalid_request", "unknown language");
      s().language = l;
      s().keep = true; // the console reloads right after: keep the demo's data
      return ok({ language: l });
    });
    a("GET", "/v1/account/tokens", "user", (c) => ok({ items: s().myTokens[c.who!.id] ?? [] }));
    a("POST", "/v1/account/tokens", "user", (c) => this.issue(c, (s().myTokens[c.who!.id] ??= [])));
    a("DELETE", "/v1/account/tokens/:id", "user", (c) => {
      const list = s().myTokens[c.who!.id] ?? [];
      s().myTokens[c.who!.id] = list.filter((t) => t.id !== c.p.id);
      return ok(undefined, 204);
    });

    // What the console shows even before a login.
    a("GET", "/v1/public/bases", "public", () => ({
      status: 200,
      body: {
        items: s()
          .bases.filter((b) => b.snapshots.some((x) => x.public))
          .map((b) => ({ name: b.name, runtime: b.runtime, description: b.description, snapshots: b.snapshots.filter((x) => x.public).map(({ name, created_at }) => ({ name, created_at })) })),
        anonymous: ANON,
      },
    }));

    a("GET", "/v1/capabilities", "user", () => ({
      status: 200,
      body: {
        version: VERSION,
        node: NODES[0],
        modules: Object.keys(MODULES).filter((m) => s().modules[m] !== "off").map((m) => `${m} ${VERSION}`),
        record: s().modules.history === "on",
        pages: [
          { module: "auth", path: "/users", title: "Users" },
          { module: "audit", path: "/log", title: "Audit log" },
        ].filter((p) => s().modules[p.module] !== "off"),
      },
    }));
    a("GET", "/v1/events", "user", () => fail(404, "not_found", "events are delivered by the demo's EventSource"));

    this.branchRoutes();
    this.baseRoutes();
    this.clusterRoutes();
  }

  private issue(c: Ctx, into: APIToken[]): Reply {
    const name = String(c.body.name ?? "").trim();
    if (!name) return fail(400, "invalid_request", "a token needs a name");
    const ttl = c.body.ttl ? parseDuration(String(c.body.ttl)) : null;
    if (c.body.ttl && ttl === null) return fail(400, "invalid_request", "invalid ttl");
    const t: APIToken = { id: "tk-" + newId(this.state).slice(0, 6), name, scope: String(c.body.scope ?? "branches"), created_at: new Date().toISOString(), expires_at: ttl ? new Date(Date.now() + ttl).toISOString() : null };
    into.push(t);
    return ok({ ...t, token: "dbb_demo_xxxxxxxxxxxxxxxxxxxxxxxx" }, 201);
  }

  private branchRoutes() {
    const s = () => this.state;
    const a = this.add.bind(this);

    a("GET", "/v1/branches", "user", (c) => ok({ items: this.visible(c.who) }));
    a("POST", "/v1/branches", "anon", (c) => this.createBranch(c));
    a("GET", "/v1/branches/:name", "anon", (c) => {
      const b = this.branchFor(c);
      return b ? ok(b) : notFound("branch");
    });
    a("PATCH", "/v1/branches/:name", "user", (c) => {
      const b = this.branchFor(c);
      if (!b) return notFound("branch");
      const ttl = parseDuration(String(c.body.ttl ?? ""));
      if (ttl === null) return fail(400, "invalid_request", "invalid ttl");
      b.expires_at = new Date(Date.now() + ttl).toISOString();
      this.emit("branch.updated", { name: b.name });
      return ok(b);
    });
    a("DELETE", "/v1/branches/:name", "user", (c) => this.branchAction(c, "branch.delete", "deleting", TIMING.branchDelete, { k: "branch.delete", name: c.p.name }, true));
    a("POST", "/v1/branches/:name/reset", "user", (c) => this.branchAction(c, "branch.reset", "resetting", TIMING.branchReset, { k: "branch.ready", name: c.p.name, reset: true }));
    a("POST", "/v1/branches/:name/recreate", "user", (c) => this.branchAction(c, "branch.create", "provisioning", TIMING.branchCreate, { k: "branch.ready", name: c.p.name }));
    a("GET", "/v1/branches/:name/dsn", "anon", (c) => {
      const b = this.branchFor(c);
      if (!b) return notFound("branch");
      if (b.status !== "ready") return fail(409, "conflict", `branch ${b.name} is ${b.status}`);
      const via = c.q.get("via") === "proxy" && b.proxy ? b.proxy : b.connection;
      const scheme = runtimeEngine(b.runtime) === "postgres" ? "postgres" : "mysql";
      return ok({ dsn: `${scheme}://${encodeURIComponent(via.user)}:${DEMO_PASSWORD}@${DEMO_HOST}:${via.port}/app` });
    });
    a("GET", "/v1/branches/:name/checkpoints", "user", (c) => {
      const b = this.branchFor(c);
      if (!b) return notFound("branch");
      const st = this.checkpointsOf(b.name);
      const lines = this.linesOf(st).map((l) => ({ ...l, live: l.line === st.line }));
      return ok({ items: st.list, next_cursor: null, lines, live: st.line, head: st.list.find((x) => x.current)?.name ?? null });
    });
    a("GET", "/v1/ext/history/branches/:name/events", "user", (c) => this.historyEvents(c));
    a("POST", "/v1/ext/history/branches/:name/restore", "user", (c) => this.historyRestore(c));
    a("POST", "/v1/branches/:name/checkpoints", "user", (c) => this.createCheckpoint(c));
    a("POST", "/v1/branches/:name/checkpoints/:cp/restore", "user", (c) => this.restoreCheckpoint(c));
    a("POST", "/v1/branches/:name/upgrade", "user", (c) => this.upgradeBranch(c));
    a("DELETE", "/v1/branches/:name/checkpoints/:cp", "user", (c) => this.deleteCheckpoint(c));
    a("GET", "/v1/operations", "user", () => ok({ items: s().ops.slice(-50) }));
    a("GET", "/v1/operations/:id", "anon", (c) => {
      const o = s().ops.find((x) => x.id === c.p.id);
      return o ? ok(o) : notFound("operation");
    });
  }

  private branchAction(c: Ctx, kind: string, status: string, ms: number, fx: Effect, allowBusy = false): Reply {
    const b = this.branchFor(c);
    if (!b) return notFound("branch");
    if (!allowBusy && b.status !== "ready" && !(kind === "branch.create" && b.status === "lost")) return fail(409, "conflict", `branch ${b.name} is ${b.status}`);
    const op = this.newOp(kind, "branch", b.name);
    b.status = status;
    b.operation = { id: op.id, kind };
    if (fx.k === "branch.upgrade") fx = { ...fx, op: op.id };
    this.schedule(op, ms, fx);
    this.emit("branch.updated", { name: b.name });
    return ok(op, 202);
  }

  // ---- checkpoints (a model of internal/usecase/dbbranch/checkpoint.go and restore.go) ----

  private checkpointsOf(name: string): CheckpointState {
    return (this.state.checkpoints[name] ??= { list: [], line: 0 });
  }

  private maxCheckpoints(): number {
    const v = this.state.settings.find((x) => x.scope === "core")?.settings.find((x) => x.key === "branch.max_checkpoints")?.value;
    const n = Number(v);
    return v !== undefined && v !== "" && Number.isInteger(n) && n >= 0 ? n : 20;
  }

  private checkpointStamp(at: number): string {
    return new Date(at).toISOString().replace(/[-:]/g, "").replace("T", "-").slice(0, 15);
  }

  private createCheckpoint(c: Ctx): Reply {
    const b = this.branchFor(c);
    if (!b) return notFound("branch");
    if (b.status !== "ready") return fail(409, "conflict", `branch ${b.name} is ${b.status}`);
    const st = this.checkpointsOf(b.name);
    let name = String(c.body.name ?? "");
    if (name === "") {
      name = this.checkpointStamp(Date.now());
      for (let i = 2; st.list.some((x) => x.name === name); i++) name = `${this.checkpointStamp(Date.now())}-${i}`;
    } else if (name.length > MAX_CHECKPOINT_NAME || !CHECKPOINT_NAME.test(name)) {
      return fail(400, "invalid_request", `invalid name "${name}": use 1-${MAX_CHECKPOINT_NAME} lowercase letters, digits or hyphens (not at the ends)`);
    }
    if (st.list.some((x) => x.name === name)) return fail(409, "already_exists", `checkpoint ${name} of branch ${b.name}: already exists`);
    const max = this.maxCheckpoints();
    const own = st.list.filter((x) => !x.auto).length;
    if (own >= max) return fail(409, "conflict", `conflict: branch ${b.name} has ${own} checkpoints of its own (limit ${max}, automatic checkpoints not counted); delete one first or raise the branch.max_checkpoints setting`);
    const cp: Checkpoint = { name, created_at: new Date().toISOString(), size: 96 * 1024 + (this.state.seq++ % 9) * 64 * 1024, line: st.line, auto: false, current: true };
    st.list.forEach((x) => (x.current = false));
    st.list.push(cp);
    return ok(cp, 201);
  }

  private restoreCheckpoint(c: Ctx): Reply {
    const b = this.branchFor(c);
    if (!b) return notFound("branch");
    const name = c.p.cp;
    if (name.length > MAX_CHECKPOINT_NAME || !CHECKPOINT_NAME.test(name)) return fail(400, "invalid_request", `invalid name "${name}"`);
    if (b.status !== "ready") return fail(409, "conflict", `branch ${b.name} is ${b.status}`);
    const st = this.checkpointsOf(b.name);
    if (!st.list.some((x) => x.name === name)) return notFound("checkpoint");
    const ms = runtimeEngine(b.runtime) === "postgres" ? TIMING.restorePostgres : TIMING.restoreMysql;
    return this.branchAction(c, "branch.restore", "restoring", ms, { k: "branch.restore", name: b.name, checkpoint: name });
  }

  // upgradeBranch is a model of internal/usecase/dbbranch/upgrade.go: the
  // same refusals, a simulated pre-check, the automatic checkpoint.
  private upgradeBranch(c: Ctx): Reply {
    const b = this.branchFor(c);
    if (!b) return notFound("branch");
    const body = c.body as { runtime?: string; check_only?: boolean; force?: boolean };
    const to = String(body.runtime ?? "");
    if (!/^(mysql|postgres)@[0-9]+\.[0-9]+(\.[0-9]+)?$/.test(to)) return fail(400, "invalid_request", `invalid runtime "${to}": want engine@version, e.g. mysql@8.4.13`);
    if (b.status !== "ready") return fail(409, "conflict", `branch ${b.name} is ${b.status}`);
    const problem = upgradeProblem(b.runtime, to);
    if (problem) return fail(422, "upgrade_not_allowed", `upgrade not allowed: ${b.runtime} to ${to}: ${problem}`);
    if (!this.state.installed.includes(to)) return fail(404, "runtime_not_installed", `runtime not installed: ${to} is not installed on node ${b.node}; install it with: db-branch runtime install ${to}`);
    const checkOnly = !!body.check_only;
    const pre: Finding[] = [
      { level: "info", code: "upgrade_path", message: `${b.runtime} to ${to} on node ${b.node}` },
      { level: "info", code: "disk_space", message: "The upgrade needs about 512.0 MiB of free space; the pool has 18.8 GiB." },
    ];
    if (b.runtime.startsWith("mysql") && /^mysql@8\.4/.test(to)) pre.push({ level: "warning", code: "native_password_users", message: "1 account(s) use mysql_native_password, which MySQL 8.4 disables by default: 'legacyapp'@'%'. db-branch keeps the plugin loaded on this branch so they still work." });
    const checkpoint = `before-upgrade-${this.checkpointStamp(Date.now())}`;
    const result: UpgradeResult = { branch: b.name, from: b.runtime, to, status: checkOnly ? "checked" : "upgraded", check_only: checkOnly, checkpoint: checkOnly ? undefined : checkpoint, pre_check: pre, log: [] };
    if (checkOnly) {
      const op = this.newOp("branch.upgrade", "branch", b.name);
      this.schedule(op, 700, { k: "op.result", op: op.id, result });
      return ok(op, 202);
    }
    result.log = [{ level: "info", code: "log_upgrade", message: `MY-013381: Server upgrade from '${b.runtime}' to '${to}' completed.` }, { level: "warning", code: "log_warning", message: "MY-010068: CA certificate ca.pem is self signed." }];
    if (!b.runtime.startsWith("mysql")) result.log = [];
    return this.branchAction(c, "branch.upgrade", "upgrading", TIMING.restoreMysql, { k: "branch.upgrade", name: b.name, to, op: "", result });
  }

  // finishUpgrade saves the state before as an automatic checkpoint (it records
  // the old runtime) and switches the branch to the new runtime.
  private finishUpgrade(b: Branch, to: string, checkpoint: string, now: number) {
    const st = this.checkpointsOf(b.name);
    st.list.forEach((x) => (x.current = false));
    st.list.push({ name: checkpoint, created_at: new Date(now).toISOString(), size: 120 * 1024, line: st.line, auto: true, current: false, runtime: b.runtime });
    const autos = st.list.filter((x) => x.auto).sort((x, y) => Date.parse(x.created_at) - Date.parse(y.created_at) || (x.name < y.name ? -1 : 1));
    const drop = new Set(autos.slice(0, Math.max(0, autos.length - MAX_AUTO_CHECKPOINTS)).map((x) => x.name));
    st.list = st.list.filter((x) => !drop.has(x.name));
    b.base_runtime = b.base_runtime ?? b.runtime;
    b.runtime = to;
    b.status = "ready";
    b.operation = null;
  }

  // finishRestore starts a new line and marks the restored checkpoint current.
  // The state the branch leaves is not saved: the person saves a checkpoint first.
  private finishRestore(b: Branch, checkpoint: string, replay?: { line: number; gtid: string }) {
    const st = this.checkpointsOf(b.name);
    const lines = this.linesOf(st);
    st.list.forEach((x) => (x.current = false));
    st.line = Math.max(st.line, ...st.list.map((x) => x.line), ...lines.map((l) => l.line)) + 1;
    const from = st.list.find((x) => x.name === checkpoint);
    st.lines = [...lines, { line: st.line, parent: from ? from.line : 0, from: from ? checkpoint : "reset", ...(replay ? { replay } : {}) }];
    if (from) from.current = true;
    b.status = "ready";
    b.operation = null;
  }

  // resetCheckpoints: a reset keeps the checkpoints and leaves the line the data was on.
  private resetCheckpoints(name: string) {
    const st = this.state.checkpoints[name];
    if (!st || st.list.length === 0) return;
    st.list.forEach((x) => (x.current = false));
    const lines = this.linesOf(st);
    st.line = Math.max(st.line, ...st.list.map((x) => x.line), ...lines.map((l) => l.line)) + 1;
    st.lines = [...lines, { line: st.line, parent: 0, from: "reset" }];
  }

  // linesOf: the lines of history. A state saved before they were recorded
  // has none: they are read from the checkpoints, as roots.
  private linesOf(st: CheckpointState): LineMeta[] {
    const known = st.lines ?? [];
    const numbers = new Set<number>([0, st.line, ...st.list.map((x) => x.line)]);
    const out = [...known];
    for (const n of [...numbers].sort((a, b) => a - b)) if (!out.some((l) => l.line === n)) out.push({ line: n, parent: null, from: "" });
    return out.sort((a, b) => a.line - b.line);
  }

  // ---- the history module (a model of the paid module: made-up transactions) ----

  private historyBranch(c: Ctx): Branch | Reply {
    const b = this.branchFor(c);
    if (!b) return notFound("branch");
    if (this.state.modules.history !== "on") return notFound("module history");
    return b;
  }

  // fakeTxs: the transactions of one line. A line made by a restore to a
  // transaction starts with the ones up to it, replayed (the same GTIDs, as the
  // module's replay keeps them), and its own follow.
  private fakeTxs(b: Branch, line: number): HistoryTx[] {
    const own = this.ownFakeTxs(b, line);
    const meta = this.linesOf(this.checkpointsOf(b.name)).find((l) => l.line === line);
    if (!meta?.replay) return own;
    const parent = this.fakeTxs(b, meta.replay.line);
    const upto = parent.findIndex((x) => x.gtid === meta.replay!.gtid);
    const replayed = upto < 0 ? [] : parent.slice(0, upto + 1);
    const last = replayed.length > 0 ? Date.parse(replayed[replayed.length - 1].time) : 0;
    return [...replayed, ...own.filter((x) => Date.parse(x.time) > last)];
  }

  // ownFakeTxs invents the transactions a line makes itself: a steady trickle
  // between its birth and its end (the next restore, or now), the same for the
  // same branch.
  private ownFakeTxs(b: Branch, line: number): HistoryTx[] {
    const st = this.checkpointsOf(b.name);
    if (!b.record || runtimeEngine(b.runtime) !== "mysql") return [];
    const mine = st.list.filter((x) => x.line === line).map((x) => Date.parse(x.created_at));
    const live = line === st.line;
    const end = live ? Date.now() : Math.max(...mine, Date.parse(b.created_at));
    const start = line === 0 ? Math.min(...mine, end) - 50 * 60e3 : Math.min(...mine, end) - 22 * 60e3;
    const count = 15;
    const out: HistoryTx[] = [];
    let seed = (b.name.length * 31 + line * 977) >>> 0;
    const rnd = () => ((seed = (seed * 1664525 + 1013904223) >>> 0) / 2 ** 32);
    for (let i = 0; i < count; i++) {
      const at = start + ((end - start) * (i + 0.2 + rnd() * 0.6)) / count;
      const id = 1000 + Math.floor(rnd() * 8000);
      const qty = 1 + Math.floor(rnd() * 5);
      const kind = Math.floor(rnd() * 5);
      const ddl = line === 0 && i === 1;
      const sql = ddl
        ? "ALTER TABLE orders ADD COLUMN gift_note VARCHAR(200) NULL"
        : [
            `UPDATE orders SET status = 'shipped' WHERE id = ${id}`,
            `INSERT INTO order_items (order_id, sku, qty) VALUES (${id}, 'SKU-${qty}0${kind}', ${qty})`,
            `UPDATE inventory SET stock = stock - ${qty} WHERE sku = 'SKU-${qty}0${kind}'`,
            `INSERT INTO customers (email, name) VALUES ('user${id}@example.com', 'User ${id}')`,
            "DELETE FROM carts WHERE updated_at < NOW() - INTERVAL 30 DAY",
          ][kind];
      const table = ddl ? "orders" : sql.includes("order_items") ? "order_items" : sql.includes("inventory") ? "inventory" : sql.includes("customers") ? "customers" : sql.includes("carts") ? "carts" : "orders";
      const inserted = sql.startsWith("INSERT") ? 1 : 0;
      const updated = sql.startsWith("UPDATE") ? 1 : 0;
      const deleted = sql.startsWith("DELETE") ? 2 + Math.floor(rnd() * 5) : 0;
      out.push({
        gtid: `8d0f1c52-6a1e-11f1-9c3b-0242ac120002:${line * 100 + i + 1}`,
        time: new Date(at).toISOString(),
        sql,
        truncated: false,
        tables: [{ name: `shop.${table}`, inserted, updated, deleted }],
        inserted,
        updated,
        deleted,
        ddl,
        file: "binlog.000002",
      });
    }
    return out;
  }

  private historyEvents(c: Ctx): Reply {
    const b = this.historyBranch(c);
    if (!("name" in b)) return b;
    const st = this.checkpointsOf(b.name);
    const line = Number(c.q.get("line") ?? st.line);
    if (!this.linesOf(st).some((l) => l.line === line)) return fail(400, "invalid_request", "line: a line of this branch");
    const limit = Math.min(500, Math.max(1, Number(c.q.get("limit") ?? 100)));
    const all = this.fakeTxs(b, line).reverse();
    const items = all.slice(0, limit);
    return ok({ line, recorded: !!b.record && runtimeEngine(b.runtime) === "mysql", items, next_before: all.length > limit ? items[items.length - 1].gtid : null, retention: "168h0m0s" });
  }

  // historyRestore restores the newest checkpoint of the line before the
  // transaction (or the creation state), like the module does before it replays.
  private historyRestore(c: Ctx): Reply {
    const b = this.historyBranch(c);
    if (!("name" in b)) return b;
    const st = this.checkpointsOf(b.name);
    const line = Number(c.body.line ?? st.line);
    const tx = this.fakeTxs(b, line).find((x) => x.gtid === c.body.gtid);
    if (!tx) return fail(409, "conflict", `line ${line} has no recorded transaction ${String(c.body.gtid)}`);
    if (b.status !== "ready") return fail(409, "conflict", `branch ${b.name} is ${b.status}`);
    const base = st.list.filter((x) => x.line === line && Date.parse(x.created_at) <= Date.parse(tx.time)).pop();
    const ms = TIMING.restoreMysql + 1200;
    return this.branchAction(c, "history.restore", "restoring", ms, { k: "branch.restore", name: b.name, checkpoint: base ? base.name : "@reset", replay: { line, gtid: tx.gtid } });
  }

  private deleteCheckpoint(c: Ctx): Reply {
    const b = this.branchFor(c);
    if (!b) return notFound("branch");
    if (b.status !== "ready") return fail(409, "conflict", `branch ${b.name} is ${b.status}`);
    const st = this.checkpointsOf(b.name);
    if (!st.list.some((x) => x.name === c.p.cp)) return notFound("checkpoint");
    st.list = st.list.filter((x) => x.name !== c.p.cp);
    return ok(undefined, 204);
  }

  private createBranch(c: Ctx): Reply {
    const { who, body } = c;
    const base = this.base(String(body.base ?? ""));
    if (!base) return notFound("base");
    const ttl = body.ttl ? parseDuration(String(body.ttl)) : 7 * DAY;
    if (ttl === null) return fail(400, "invalid_request", "invalid ttl");
    let name = String(body.name ?? "");
    if (who && !who.admin) {
      const prefix = prefixOf(who);
      const typed = name.startsWith(prefix) ? name.slice(prefix.length) : name;
      if (typed.length > MAX_NAME - prefix.length) return fail(400, "invalid_request", `names made here start with ${prefix}, so the part you type may have at most ${MAX_NAME - prefix.length} characters`);
      name = prefix + typed;
    }
    if (name.length > MAX_NAME || !NAME.test(name)) return fail(400, "invalid_request", "invalid branch name: use lowercase letters, digits and single hyphens, starting with a letter (32 characters at most)");
    if (!who) {
      const latest = base.snapshots.find((x) => x.name === base.latest_snapshot);
      if (!latest?.public) return fail(403, "forbidden", `log in to branch from base ${base.name} (that snapshot is not public)`);
      if (ttl > ANON_TTL) return fail(403, "forbidden", "without logging in a branch lasts at most 4 hours");
      if (this.state.branches.filter((b) => b.owner === null).length >= ANON.max_branches) return fail(429, "rate_limited", `at most ${ANON.max_branches} branches may exist without logging in`);
    }
    if (this.state.branches.some((b) => b.name === name)) return fail(409, "already_exists", `branch ${name}: already exists; names are unique across the cluster, so pick another name or delete the existing branch first`);
    if (!base.latest_snapshot) return fail(409, "conflict", `base ${base.name} has no snapshot yet`);

    const pg = runtimeEngine(base.runtime) === "postgres";
    const n = this.state.seq++;
    const op = this.newOp("branch.create", "branch", name);
    const b: Branch = {
      name,
      status: "provisioning",
      status_reason: null,
      base: base.name,
      snapshot: base.latest_snapshot,
      runtime: base.runtime,
      node: NODES[n % NODES.length],
      created_at: new Date().toISOString(),
      expires_at: new Date(Date.now() + ttl).toISOString(),
      reset_at: null,
      storage: null,
      connection: { host: DEMO_HOST, port: 41100 + (n % 800), user: pg ? name : "branch" },
      proxy: { host: DEMO_HOST, port: pg ? 6432 : 6033, user: name },
      operation: { id: op.id, kind: op.kind },
      owner: who ? (who.admin && body.owner ? String(body.owner) : who.id) : null,
      id: newId(this.state),
      access: !who || body.access === "open" ? "open" : "private",
      // Decided here, once: the branch asked to, or record_all is on right now.
      record: (body.record === true || this.recordAll()) && !!who && !pg && this.state.modules.history === "on" ? true : undefined,
      checkpoints: true,
    };
    this.state.branches.push(b);
    this.schedule(op, TIMING.branchCreate, { k: "branch.ready", name });
    this.emit("branch.updated", { name });
    return ok(op, 202);
  }

  // recordAll is the history module's record_all setting as it is now.
  private recordAll(): boolean {
    const set = this.scopes().find((x) => x.scope === "history")?.settings.find((x) => x.key === "record_all");
    return set?.value === "true";
  }

  private baseRoutes() {
    const s = () => this.state;
    const a = this.add.bind(this);
    const named = (run: (b: BaseRec, c: Ctx) => Reply) => (c: Ctx) => {
      const b = this.base(c.p.name);
      return b ? run(b, c) : notFound("base");
    };
    const idle = (b: BaseRec, run: () => Reply) => (b.operation || b.status !== "ready" ? fail(409, "conflict", `base ${b.name} is busy (${b.operation?.kind ?? b.status})`) : run());
    const busy = (b: BaseRec, kind: string, status: string, ms: number, fx: Effect): Reply => {
      const op = this.newOp(kind, "base", b.name);
      b.status = status;
      b.operation = { id: op.id, kind };
      this.schedule(op, ms, fx);
      this.emit("base.updated", { name: b.name });
      return ok(op, 202);
    };
    const dumpSize = (dump: unknown) => (typeof dump === "string" && dump.startsWith("upload:") ? (s().uploads.find((u) => u.id === dump)?.size ?? 0) : 64 * MIB);
    const work = (size: number) => TIMING.baseCreate + Math.min((size / MIB) * 20, 3500);

    a("GET", "/v1/bases", "user", (c) => ok({ items: s().bases.map((b) => this.baseView(b, c.who)) }));
    a("POST", "/v1/bases", "admin", (c) => {
      const { name, runtime, dump, description } = c.body as { name?: string; runtime?: string; dump?: string; description?: string; node?: string };
      if (!name || name.length > MAX_NAME || !NAME.test(name)) return fail(400, "invalid_request", "invalid base name: use lowercase letters, digits and single hyphens, starting with a letter");
      if (!runtime || !s().installed.includes(runtime)) return fail(404, "runtime_not_installed", `runtime ${runtime ?? ""} is not installed`);
      if (this.base(name)) return fail(409, "already_exists", `base ${name} already exists`);
      const node = c.body.node ? String(c.body.node) : NODES[0];
      if (!NODES.includes(node)) return fail(400, "invalid_request", `no node ${node}`);
      if (typeof dump === "string" && dump.startsWith("upload:") && !s().uploads.some((u) => u.id === dump)) return notFound("upload");
      const size = dump ? dumpSize(dump) : 0;
      const rec: BaseRec = {
        name,
        runtime,
        status: "provisioning",
        status_reason: null,
        node,
        port: Math.max(3306, ...s().bases.map((b) => b.port)) + 1,
        created_at: new Date().toISOString(),
        latest_snapshot: null,
        snapshots: [],
        replica_nodes: (c.body.replica_nodes as string[] | null | undefined) ?? null,
        operation: null,
        description: description ?? "",
        size,
        refresh: null,
      };
      s().bases.push(rec);
      return busy(rec, "base.create", "provisioning", work(size), { k: "base.ready", name, snapshot: !!dump, grow: 0 });
    });
    a("PATCH", "/v1/bases/:name", "admin", named((b, c) => {
      const d = String(c.body.description ?? "");
      if (d.length > 1000) return fail(400, "invalid_request", "description is too long");
      b.description = d;
      this.emit("base.updated", { name: b.name });
      return ok(this.baseView(b, c.who));
    }));
    a("DELETE", "/v1/bases/:name", "admin", named((b) => {
      if (b.operation) return fail(409, "conflict", `base ${b.name} is busy`);
      if (s().branches.some((x) => x.base === b.name)) return fail(409, "in_use", `base ${b.name} still has branches`);
      const op = this.newOp("base.delete", "base", b.name);
      b.operation = { id: op.id, kind: op.kind };
      this.schedule(op, TIMING.baseDelete, { k: "base.delete", name: b.name });
      this.emit("base.updated", { name: b.name });
      return ok(op, 202);
    }));
    a("POST", "/v1/bases/:name/snapshots", "admin", named((b) => idle(b, () => busy(b, "base.snapshot", "maintenance", TIMING.baseSnapshot, { k: "base.ready", name: b.name, snapshot: true }))));
    a("POST", "/v1/bases/:name/load", "admin", named((b, c) => {
      const dump = String(c.body.dump ?? "");
      if (!dump) return fail(400, "invalid_request", "dump is empty");
      if (dump.startsWith("upload:") && !s().uploads.some((u) => u.id === dump)) return notFound("upload");
      const size = dumpSize(dump);
      return idle(b, () => busy(b, "base.load", "maintenance", work(size), { k: "base.ready", name: b.name, snapshot: !!c.body.snapshot, grow: size }));
    }));
    // The demo runs no SQL: it checks that there is some and says it worked.
    a("POST", "/v1/bases/:name/sql", "admin", named((_b, c) => (String(c.body.sql ?? "").trim() ? ok(undefined, 204) : fail(400, "invalid_request", "sql is empty"))));
    a("PATCH", "/v1/bases/:name/snapshots/:snapshot", "admin", named((b, c) => {
      const snap = b.snapshots.find((x) => x.name === c.p.snapshot);
      if (!snap) return notFound("snapshot");
      snap.public = !!c.body.public;
      this.emit("base.updated", { name: b.name });
      return ok(this.baseView(b, c.who));
    }));
    a("DELETE", "/v1/bases/:name/snapshots/:snapshot", "admin", named((b, c) => {
      const snap = b.snapshots.find((x) => x.name === c.p.snapshot);
      if (!snap) return notFound("snapshot");
      if (snap.name === b.latest_snapshot) return fail(409, "conflict", "the latest snapshot is kept");
      if (s().branches.some((x) => x.base === b.name && x.snapshot === snap.name)) return fail(409, "in_use", "branches use this snapshot");
      b.snapshots = b.snapshots.filter((x) => x !== snap);
      this.emit("base.updated", { name: b.name });
      return ok(undefined, 204);
    }));
    a("PUT", "/v1/bases/:name/replicas", "admin", named((b, c) => {
      const list = c.body.replica_nodes as string[] | null | undefined;
      if (list != null && (!Array.isArray(list) || list.some((n) => !NODES.includes(n)))) return fail(400, "invalid_request", "unknown node");
      b.replica_nodes = list ?? null;
      this.emit("base.updated", { name: b.name });
      return ok(this.baseView(b, c.who));
    }));
    a("GET", "/v1/bases/:name/refresh", "admin", named((b) => (b.refresh ? ok(b.refresh) : fail(404, "not_found", `base ${b.name} has no refresh schedule`))));
    a("PUT", "/v1/bases/:name/refresh", "admin", named((b, c) => {
      const src = (c.body.source ?? {}) as { host?: string; port?: number; user?: string; password?: string; databases?: string[] };
      const every = parseDuration(String(c.body.every ?? ""));
      if (!src.host || !src.user || !src.databases?.length) return fail(400, "invalid_request", "the source needs a host, a user and databases");
      if (every === null || every < 3600e3) return fail(400, "invalid_request", "refresh at most hourly: every must be 1h or more");
      const r: BaseRefresh = {
        base: b.name,
        engine: runtimeEngine(b.runtime),
        source: { host: src.host, port: src.port ?? 0, user: src.user, password_set: !!src.password || !!b.refresh?.source.password_set, databases: src.databases },
        every: goDuration(every),
        updated_at: new Date().toISOString(),
        last_run: b.refresh?.last_run ?? null,
        next_run: new Date(Date.now() + every).toISOString(),
        operation: null,
      };
      b.refresh = r;
      this.emit("base.updated", { name: b.name });
      return ok(r);
    }));
    a("DELETE", "/v1/bases/:name/refresh", "admin", named((b) => ((b.refresh = null), this.emit("base.updated", { name: b.name }), ok(undefined, 204))));
    a("POST", "/v1/bases/:name/refresh/run", "admin", named((b) => {
      if (!b.refresh) return fail(404, "not_found", `base ${b.name} has no refresh schedule`);
      return idle(b, () => busy(b, "base.refresh", "maintenance", TIMING.refresh, { k: "refresh.run", name: b.name }));
    }));

    // Dump uploads: only the name and the size count; the file is never read.
    a("GET", "/v1/uploads/room", "admin", (c) => ok({ node: c.q.get("node") || NODES[0], max_bytes: this.uploadMax(), reason: "limit" }));
    a("POST", "/v1/uploads", "admin", (c) => {
      const size = Number(c.body.size);
      const name = c.q.get("name") ?? "";
      if (!(size > 0)) return fail(411, "length_required", "send the file with a Content-Length");
      if (!name) return fail(400, "invalid_request", "name is empty");
      if (size > this.uploadMax()) return fail(413, "too_large", `the dump is ${size} bytes; the node accepts at most ${this.uploadMax()}`);
      const u: Upload = { id: "upload:" + newId(s()).slice(0, 12), name, size, node: c.q.get("node") || NODES[0] };
      s().uploads.push(u);
      return ok(u, 201);
    });
  }

  private uploadMax(): number {
    const v = this.state.settings.flatMap((x) => x.settings).find((x) => x.key === "upload.max_mb");
    return Number(v?.value || v?.default || 20480) * MIB;
  }

  private clusterRoutes() {
    const s = () => this.state;
    const a = this.add.bind(this);

    a("GET", "/v1/cluster", "admin", () => {
      const size = [100, 60, 60];
      const free = [47.9, 28.5, 31.2];
      return ok({
        mode: "cluster",
        leader: NODES[0],
        protocol: PROTOCOL,
        nodes: NODES.map((name, i) => ({
          name,
          host: `${name}.${DEMO_HOST}`,
          version: VERSION,
          leader: i === 0,
          voter: true,
          down: false,
          last_seen: new Date().toISOString(),
          pool_size: size[i] * GIB,
          pool_free: Math.round(free[i] * GIB),
          arc_max_mb: 2048,
          runtimes: s().installed,
          protocol: PROTOCOL,
          metrics: null,
          system: {
            distro: "ubuntu-24.04",
            os: "Ubuntu 24.04.3 LTS",
            kernel: ["6.8.0-87-generic", "6.8.0-87-generic", "6.8.0-85-generic"][i],
            zfs: "2.2.2-0ubuntu9.4",
          },
        })),
      });
    });
    a("GET", "/v1/nodes/:name/metrics", "admin", (c) => {
      const i = NODES.indexOf(c.p.name);
      if (i < 0) return notFound("node");
      const now = Date.now();
      return ok({
        items: Array.from({ length: 61 }, (_, k) => {
          const age = 60 - k;
          const w = Math.sin((age + i * 7) / 6);
          return {
            time: new Date(now - age * 60e3).toISOString(),
            cpu: 8 + i * 3 + 6 * (w + 1) + (age % 5),
            load1: 0.3 + 0.1 * w,
            mem_total: 8 * GIB,
            mem_used: (2.4 + i * 0.3 + 0.15 * w) * GIB,
            net_rx: (180 + 90 * w + i * 40) * 1024,
            net_tx: (120 + 60 * w + i * 30) * 1024,
            arc: 1.2 * GIB,
          };
        }),
      });
    });

    a("GET", "/v1/runtimes", "admin", () => ok({ items: s().installed.map((runtime) => ({ runtime, engine: runtimeEngine(runtime), version: runtime.split("@")[1] })) }));
    a("GET", "/v1/runtimes/available", "admin", () => ok({ items: s().available }));
    a("POST", "/v1/runtimes", "admin", (c) => {
      const runtime = String(c.body.runtime ?? "");
      if (s().installed.includes(runtime) || s().pending.some((p) => p.fx.k === "runtime.install" && p.fx.runtime === runtime)) return fail(409, "already_exists", `runtime ${runtime} is already installed`);
      if (!s().available.some((r) => r.runtime === runtime)) return fail(422, "runtime_unavailable", `runtime ${runtime} is not in the package repositories`);
      const op = this.newOp("runtime.install", "runtime", runtime);
      this.schedule(op, TIMING.runtime, { k: "runtime.install", runtime });
      return ok(op, 202);
    });
    a("DELETE", "/v1/runtimes/:engine/:version", "admin", (c) => {
      const runtime = `${c.p.engine}@${c.p.version}`;
      if (!s().installed.includes(runtime)) return fail(404, "runtime_not_installed", `runtime ${runtime} is not installed`);
      if (s().bases.some((b) => b.runtime === runtime) || s().branches.some((b) => b.runtime === runtime)) return fail(409, "in_use", `runtime ${runtime} is in use`);
      s().installed = s().installed.filter((r) => r !== runtime);
      s().available = [...s().available, { runtime, engine: c.p.engine, version: c.p.version }];
      this.emit("runtime.updated", { runtime });
      return ok(undefined, 204);
    });

    a("GET", "/v1/settings", "admin", () => ok({ items: this.scopes() }));
    a("PUT", "/v1/settings/:scope", "admin", (c) => {
      const scope = this.scopes().find((x) => x.scope === c.p.scope);
      if (!scope) return notFound("settings scope");
      const values = (c.body.values ?? {}) as Record<string, string>;
      const next: Record<string, string> = {};
      for (const [key, raw] of Object.entries(values)) {
        const set = scope.settings.find((x) => x.key === key);
        if (!set) return fail(400, "invalid_request", `unknown setting ${key}`);
        const v = String(raw ?? "").trim();
        const bad = (v !== "" && set.type === "int" && !/^-?\d+$/.test(v)) || (v !== "" && set.type === "bool" && v !== "true" && v !== "false") || (v !== "" && set.type === "duration" && parseDuration(v) === null);
        if (bad) return fail(400, "invalid_request", `${set.label}: ${v} is not a valid ${set.type}`);
        next[key] = v === "" ? set.default : set.type === "duration" ? goDuration(parseDuration(v)!) : v;
      }
      for (const set of scope.settings) if (set.key in next) set.value = next[set.key];
      this.emit("resync", {});
      return ok(scope);
    });

    a("GET", "/v1/modules", "admin", () => ok({ items: Object.keys(MODULES).map((m) => this.moduleView(m)), available: Object.fromEntries(Object.entries(MODULES).map(([k, v]) => [k, v.description])) }));
    a("POST", "/v1/modules", "admin", (c) => {
      const name = String(c.body.name ?? "");
      if (!(name in MODULES)) return notFound("module");
      if (s().modules[name] === "off") {
        s().modules[name] = "installing";
        this.schedule(null, TIMING.module, { k: "module.ready", name });
        this.emit("resync", {});
      }
      return ok(this.moduleView(name), 201);
    });
    a("DELETE", "/v1/modules/:name", "admin", (c) => {
      if (!(c.p.name in MODULES)) return notFound("module");
      s().modules[c.p.name] = "off";
      this.emit("resync", {});
      return ok(undefined, 204);
    });

    // Licence: the demo is in its trial and cannot check a key.
    a("GET", "/v1/license", "admin", () =>
      ok({
        configured: false,
        report: {
          license: { id: "trial", customer: "", modules: [], max_nodes: 0, expires: new Date(Date.now() + 23 * DAY).toISOString(), grace_days: 0 },
          state: "valid",
          trial: true,
          all: true,
          modules: [],
          unavailable: [],
          enabled: [],
        },
      }),
    );
    const licence = (c: Ctx) => (String(c.body.key ?? "").trim() ? fail(422, "invalid_license", "licence keys cannot be checked in the demo") : fail(400, "invalid_request", "key is empty"));
    a("POST", "/v1/license/verify", "admin", licence);
    a("PUT", "/v1/license", "admin", licence);

    a("GET", "/v1/updates", "admin", (c) => ok(this.updates(c.who!.id)));
    a("POST", "/v1/updates/check", "admin", (c) => {
      s().checkedAt = new Date().toISOString();
      return ok(this.updates(c.who!.id));
    });
    a("POST", "/v1/updates", "admin", () => fail(409, "conflict", "the demo cluster is already on the newest version"));
    a("DELETE", "/v1/updates", "admin", () => ok(undefined, 204));

    a("GET", "/v1/tokens", "admin", () => ok({ items: s().tokens }));
    a("POST", "/v1/tokens", "admin", (c) => this.issue(c, s().tokens));
    a("DELETE", "/v1/tokens/:id", "admin", (c) => ((s().tokens = s().tokens.filter((t) => t.id !== c.p.id)), ok(undefined, 204)));

    a("GET", "/v1/backups", "admin", (c) => {
      const node = c.q.get("node") || NODES[0];
      return NODES.includes(node) ? ok({ node, items: s().backups[node] }) : notFound("node");
    });
    a("POST", "/v1/backups", "admin", (c) => {
      const node = c.q.get("node") || NODES[0];
      if (!NODES.includes(node)) return notFound("node");
      const f = { name: `backup-${node}-${snapshotName(Date.now())}.tar.zst`, size: 391_000 + s().backups[node].length * 97, taken_at: new Date().toISOString() };
      s().backups[node] = [f, ...s().backups[node]];
      return ok(f, 201);
    });
  }

  private updates(by: string) {
    const s = this.state;
    return {
      checked_at: s.checkedAt,
      checked_by: by,
      nodes: NODES.map((name, i) => ({ name, leader: i === 0, down: false, version: VERSION, modules: Object.fromEntries(Object.keys(MODULES).filter((m) => s.modules[m] !== "off").map((m) => [m, VERSION])) })),
      packages: [{ name: "db-branch", installed: VERSION, available: [] }],
      latest_minor: "",
      rollout: null,
    };
  }
}

const prefixOf = (who: Who) => ownerPrefix(who.id) + "-";

function decodeParams(g: Record<string, string>): Record<string, string> {
  return Object.fromEntries(Object.entries(g).map(([k, v]) => [k, decodeURIComponent(v)]));
}
