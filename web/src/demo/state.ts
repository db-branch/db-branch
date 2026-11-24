// The fake cluster's state: plain data (JSON-safe), so a tab can keep it across
// page loads. Nothing here talks to a network; see backend.ts for the API.
import type { UpgradeResult } from "../lib/upgrade";
import type { APIToken, Base, BaseRefresh, BackupFile, Branch, Checkpoint, HistoryTx, MergeResult, Operation, Runtime, SettingScope, Upload } from "../api";

export const MIB = 1024 * 1024;
export const GIB = 1024 * MIB;

export const DEMO_HOST = "demo.invalid"; // RFC 6761: never resolves
export const DEMO_PASSWORD = "demo";
export const NODES = ["node-1", "node-2", "node-3"];
export const VERSION = "0.1.0";
export const PROTOCOL = 13;

export type Who = { id: string; name: string; roles: string[]; admin: boolean };

export const ADMIN: Who = { id: "demo-admin", name: "demo-admin", roles: ["admin"], admin: true };
export const DEVELOPER: Who = { id: "alex@example.com", name: "alex@example.com", roles: ["developer"], admin: false };

// BaseRec is a base as stored: the API's view adds replicas, holders and can.
export type BaseRec = Omit<Base, "replicas" | "holders" | "can"> & { size: number; refresh: BaseRefresh | null };

// Effect is what happens when a simulated operation finishes.
export type Effect =
  | { k: "branch.ready"; name: string; reset?: boolean }
  | { k: "branch.paused"; name: string }
  | { k: "branch.delete"; name: string }
  | { k: "branch.restore"; name: string; checkpoint: string; replay?: { line: number; gtid: string; txs: HistoryTx[] } }
  | { k: "branch.upgrade"; name: string; to: string; op: string; result: UpgradeResult }
  | { k: "branch.merge"; name: string; op: string; result: MergeResult }
  | { k: "op.result"; op: string; result: UpgradeResult }
  | { k: "base.ready"; name: string; snapshot: boolean; grow?: number }
  | { k: "base.delete"; name: string }
  | { k: "refresh.run"; name: string }
  | { k: "runtime.install"; runtime: string }
  | { k: "module.ready"; name: string };

export type Pending = { op: string; due: number; fx: Effect };

// Checkpoints of one branch, oldest first; line is the line of history the
// branch's live data is on (a restore, or a reset that keeps checkpoints,
// starts a new one).
export type CheckpointState = { list: Checkpoint[]; line: number; lines?: LineMeta[] };

// Where a line of history came from: the checkpoint it was restored from
// ("reset": the creation state) on line parent. The first line has none.
// replay: a line made by a restore to a transaction starts with the transactions up to it (the module replays them).
export type LineMeta = { line: number; parent: number | null; from: string; replay?: { line: number; gtid: string; txs: HistoryTx[] } };

export type State = {
  seq: number;
  user: Who | null;
  csrf: string;
  language: string;
  // keep: the next reload (a language switch) keeps this state.
  keep: boolean;
  installed: string[];
  available: Runtime[];
  bases: BaseRec[];
  branches: Branch[];
  // checkpoints by branch name; absent in states saved before they existed.
  checkpoints: Record<string, CheckpointState>;
  ops: Operation[];
  pending: Pending[];
  uploads: Upload[];
  tokens: APIToken[];
  myTokens: Record<string, APIToken[]>;
  settings: SettingScope[];
  // modules: on, installing on the nodes (a moment after enabling), or off.
  modules: Record<string, "on" | "installing" | "off">;
  backups: Record<string, BackupFile[]>;
  checkedAt: string;
};

export const MODULES: Record<string, { description: string; paid: boolean }> = {
  audit: { description: "Audit log of every change", paid: true },
  auth: { description: "Users, roles, teams and single sign-on", paid: true },
  history: { description: "Record every change and restore a branch to any point", paid: true },
  proxysql: { description: "Connect to branches through ProxySQL", paid: false },
};

export const runtimeEngine = (runtime: string) => runtime.split("@")[0];

const iso = (ms: number) => new Date(ms).toISOString();

// snapshotName is the name a snapshot taken at ms gets (UTC, to the second).
export const snapshotName = (ms: number) => "s" + iso(ms).replace(/[-:T]/g, "").slice(0, 14);

function snaps(now: number, hours: number[], publicLast = false) {
  return hours
    .map((h, i) => ({ h, i }))
    .sort((a, b) => b.h - a.h)
    .map(({ h }, i, all) => ({ name: snapshotName(now - h * 3600e3), created_at: iso(now - h * 3600e3), public: publicLast && i === all.length - 1 }));
}

function base(now: number, name: string, runtime: string, node: string, port: number, description: string, hours: number[], size: number, publicLast = false): BaseRec {
  const snapshots = snaps(now, hours, publicLast);
  return {
    name,
    runtime,
    status: "ready",
    status_reason: null,
    node,
    port,
    created_at: iso(now - 20 * 24 * 3600e3),
    latest_snapshot: snapshots[snapshots.length - 1].name,
    snapshots,
    replica_nodes: null,
    operation: null,
    description,
    size,
    refresh: null,
  };
}

const hex = (n: number) => (Math.imul(n + 1, 0x9e3779b1) >>> 0).toString(16).padStart(8, "0") + (Math.imul(n + 7, 0x85ebca6b) >>> 0).toString(16).padStart(8, "0");
export const newId = (s: State) => hex(s.seq++);

function branch(s: State, now: number, name: string, b: BaseRec, owner: string, usedMiB: number, expiresInH: number, ageH: number, access: "private" | "open" = "private"): Branch {
  const pg = b.runtime.startsWith("postgres");
  const n = s.branches.length;
  return {
    name,
    status: "ready",
    status_reason: null,
    base: b.name,
    snapshot: b.latest_snapshot!,
    runtime: b.runtime,
    node: NODES[n % NODES.length],
    created_at: iso(now - ageH * 3600e3),
    expires_at: iso(now + expiresInH * 3600e3),
    reset_at: null,
    storage: { used: Math.round(usedMiB * MIB), referenced: Math.round(usedMiB * MIB * 1.12) },
    connection: { host: DEMO_HOST, port: 41001 + n, user: pg ? `${name}` : "branch" },
    proxy: pg ? null : { host: DEMO_HOST, port: 6033, user: name },
    operation: null,
    owner,
    owner_name: owner,
    id: newId(s),
    access,
    checkpoints: true,
  };
}

const iv = (key: string, label: string, type: SettingScope["settings"][number]["type"], def: string, help?: string) => ({ key, label, help, type, default: def, value: def });

export function seed(now: number): State {
  const s = { seq: 1 } as State;
  const storefront = base(now, "storefront", "postgres@18.6", "node-1", 5433, "Storefront catalog, carts and checkout (about 1 GiB)", [70, 46, 22, 5], 1 * GIB, true);
  const orders = base(now, "orders", "mysql@8.4.13", "node-2", 3307, "Order history and fulfilment, refreshed from a read replica", [52, 28, 9], 320 * MIB);
  orders.refresh = {
    base: "orders",
    engine: "mysql",
    source: { host: "replica.db.demo.invalid", port: 3306, user: "readonly", password_set: true, databases: ["orders"] },
    every: "24h0m0s",
    updated_at: iso(now - 9 * 24 * 3600e3),
    last_run: { started_at: iso(now - 9 * 3600e3), finished_at: iso(now - 9 * 3600e3 + 95e3), ok: true, error: null },
    next_run: iso(now + 15 * 3600e3),
    operation: null,
  };
  Object.assign(s, {
    user: null,
    csrf: "demo-csrf",
    language: "",
    keep: false,
    installed: ["mysql@8.4.13", "postgres@18.6"],
    available: [
      { runtime: "postgres@17.6", engine: "postgres", version: "17.6" },
      { runtime: "mysql@9.4.0", engine: "mysql", version: "9.4.0" },
      { runtime: "mysql@8.0.43", engine: "mysql", version: "8.0.43" },
    ],
    bases: [storefront, orders],
    branches: [],
    checkpoints: {},
    ops: [],
    pending: [],
    uploads: [],
    tokens: [
      { id: "tk-1", name: "ci-runner", scope: "branches", created_at: iso(now - 9 * 24 * 3600e3), expires_at: iso(now + 21 * 24 * 3600e3) },
      { id: "tk-2", name: "release-bot", scope: "full", created_at: iso(now - 40 * 24 * 3600e3), expires_at: null },
    ],
    myTokens: { "alex@example.com": [{ id: "mt-1", name: "my-laptop", scope: "branches", created_at: iso(now - 3 * 24 * 3600e3), expires_at: iso(now + 27 * 24 * 3600e3) }] },
    settings: [
      {
        scope: "core",
        title: "General",
        settings: [
          iv("branch.default_ttl", "Default TTL", "duration", "", "Lifetime of a branch made without one (empty: 7 days)."),
          iv("branch.memory_mb", "Branch memory (MB)", "int", "512"),
          iv("branch.max_checkpoints", "Checkpoints per branch", "int", "20", "The most checkpoints you take yourself that one branch may hold. Automatic checkpoints (taken before an upgrade) do not count (the newest five are kept)."),
          iv("branch.anonymous_ttl", "Anonymous branch TTL", "duration", "4h0m0s"),
          iv("branch.anonymous_max", "Anonymous branches", "int", "2"),
          iv("upload.max_mb", "Largest dump upload (MB)", "int", "20480"),
          iv("backup.interval", "Backup interval", "duration", "6h0m0s", "How often every node saves the cluster state."),
          iv("backup.keep", "Backups kept", "int", "14", "Newest backups each node keeps."),
          iv("notify.expiry_warning", "Warn before expiry", "duration", "1h0m0s"),
          iv("update.auto_check", "Check for updates daily", "bool", "false"),
          iv("base.prune_snapshots", "Prune old snapshots", "bool", "true"),
          iv("license.key", "License key", "secret", ""),
        ],
      },
      { scope: "auth", title: "auth", settings: [iv("session_ttl", "Session lifetime", "duration", "12h0m0s"), iv("oidc_enabled", "Single sign-on (OIDC)", "bool", "false")] },
      { scope: "audit", title: "audit", settings: [iv("retention", "Retention", "duration", "2160h0m0s", "How long audit events are kept.")] },
      {
        scope: "history",
        title: "history",
        settings: [
          iv("record_all", "Record every branch", "bool", "false", "Record the changes of every branch created from now on, not only those created with Record. Whether a branch records is decided when it is created and never changes: branches that exist are not affected."),
          iv("retention", "History retention", "duration", "168h0m0s", "How long recorded changes are kept. Older changes cannot be restored to."),
        ],
      },
      { scope: "proxysql", title: "proxysql", settings: [iv("comment_routing", "Route by query comment", "bool", "true"), iv("server_version", "Reported MySQL version", "string", "8.4.0")] },
    ],
    modules: { audit: "on", auth: "on", history: "on", proxysql: "on" },
    backups: Object.fromEntries(NODES.map((n, i) => [n, [0, 6, 12, 18].map((h) => ({ name: `backup-${n}-${snapshotName(now - (h + i) * 3600e3)}.tar.zst`, size: 380 * 1024 + h * 211 + i * 4096, taken_at: iso(now - (h + i) * 3600e3 - 600e3) }))])),
    checkedAt: iso(now - 3600e3),
  });
  const mk = (name: string, b: BaseRec, owner: string, usedMiB: number, expiresInH: number, ageH: number, access?: "private" | "open") =>
    s.branches.push(branch(s, now, name, b, owner, usedMiB, expiresInH, ageH, access));
  mk("alex-checkout-fix", storefront, "alex@example.com", 18, 20, 3);
  mk("alex-orders-report", orders, "alex@example.com", 6, 3 * 24, 30);
  mk("feature-checkout", storefront, "demo-admin", 41, 6 * 24, 18);
  mk("migration-test", storefront, "demo-admin", 212, 2 * 24, 90);
  mk("report-fix", orders, "demo-admin", 9, 7 * 24, 4);
  const cp = (name: string, minutesAgo: number, size: number, current = false): Checkpoint => ({ name, created_at: iso(now - minutesAgo * 60e3), size, line: 0, auto: false, current });
  s.checkpoints["migration-test"] = { line: 0, list: [cp("before-migration", 80, 2.4 * MIB), cp("after-schema-change", 35, 11.8 * MIB, true)] };
  // A recorded MySQL branch with a restore history: line 0 restored to its first
  // checkpoint (line 1), then to its second (line 2, live).
  const hist = (name: string, minutesAgo: number, line: number, auto = false, current = false): Checkpoint => ({ ...cp(name, minutesAgo, 180 * 1024 + minutesAgo * 4096, current), line, auto });
  s.checkpoints["alex-orders-report"] = {
    line: 2,
    lines: [
      { line: 0, parent: null, from: "" },
      { line: 1, parent: 0, from: "baseline" },
      { line: 2, parent: 0, from: "after-import" },
    ],
    list: [hist("baseline", 150, 0), hist("after-import", 110, 0), hist("before-restore-20261112-014000", 70, 0, true), hist("retry-join", 50, 1), hist("before-restore-20261112-021000", 30, 1, true), hist("fixed-totals", 12, 2, false, true)],
  };
  s.branches.find((b) => b.name === "alex-orders-report")!.record = true;
  s.checkpoints["alex-checkout-fix"] = { line: 0, list: [cp("20261112-024500", 12, 640 * 1024, true)] };
  mk("ci-pr-482", storefront, "ci-runner", 14, 5, 2, "open");
  return s;
}
