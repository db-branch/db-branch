// Thin client for the agent's /v1 API. The session cookie is HttpOnly; the
// CSRF token from login/session is sent on every state-changing request.

export type Branch = {
  name: string;
  status: string;
  status_reason: string | null;
  base: string;
  snapshot: string;
  runtime: string;
  node: string;
  created_at: string;
  expires_at: string | null;
  reset_at: string | null;
  storage: { used: number; referenced: number } | null;
  connection: { host: string; port: number; user: string };
  proxy: { host: string; port: number; user: string } | null;
  operation: { id: string; kind: string } | null;
  owner: string | null;
  id: string;
  access: "private" | "open";
  // record: the branch was created with its changes recorded (history module).
  record?: boolean;
  // checkpoints: the node holding the branch can take checkpoints (own ZFS).
  checkpoints: boolean;
};

// Checkpoint is a saved state of a branch. line is the line of history it was
// taken on (a restore starts a new one); auto marks those taken before a
// restore; current marks the one the branch's data descends from.
export type Checkpoint = { name: string; created_at: string; size: number; line: number; auto: boolean; current: boolean; warning?: string };

// Line is a line of a branch's history: a restore starts a new one, branching
// off the checkpoint it restored (from; "reset" is the branch's creation state)
// on line parent. live is the line the branch runs on.
export type Line = { line: number; live: boolean; parent: number | null; from: string };

// CheckpointList is the checkpoints and the structure of their lines; head is
// the checkpoint the branch's data descends from.
export type CheckpointList = { items: Checkpoint[]; lines: Line[]; live: number; head: string | null };

// HistoryTx is one recorded transaction (the paid history module).
export type HistoryTx = {
  gtid: string;
  time: string;
  sql: string;
  truncated: boolean;
  tables: { name: string; inserted: number; updated: number; deleted: number }[];
  inserted: number;
  updated: number;
  deleted: number;
  ddl: boolean;
  file: string;
};
export type HistoryEvents = { line: number; recorded: boolean; items: HistoryTx[]; next_before: string | null; retention: string };

export type Base = {
  name: string;
  runtime: string;
  status: string;
  status_reason: string | null;
  node: string;
  port: number;
  created_at: string;
  latest_snapshot: string | null;
  snapshots: { name: string; created_at: string; public: boolean; mask?: { source: string; source_snapshot: string; masker: string } }[];
  replicas: Record<string, string[]>;
  // replica_nodes: null keeps a copy on every node; a list names the other nodes that do (the base's own node is implied).
  replica_nodes?: string[] | null;
  holders?: { node: string; state: "held" | "pulling" | "draining"; origin: boolean }[];
  operation: { id: string; kind: string } | null;
  description: string;
  // anonymous: what people without an account may do with the base besides the cluster-wide limits; null inherits those.
  anonymous?: { max_branches: number | null; max_ttl: string | null };
  // can: what the caller may do on this base (per-base rights); absent on older servers.
  can?: Record<string, boolean>;
};

// PublicBase is a base anyone may branch from, with what may be done now: the limits in force (the stricter of the base's and the cluster's), the anonymous branches it has and how many more fit.
export type PublicBase = {
  name: string;
  runtime: string;
  description: string;
  snapshots: { name: string; created_at: string }[];
  anonymous?: { max_branches: number; max_ttl: string; branches: number; remaining: number };
};

export type BackupFile = { name: string; size: number; taken_at: string };

export type Upload = { id: string; name: string; size: number; node: string };

// uploadDump streams a dump file to the node that will use it, reporting
// progress (0..1). fetch() cannot report upload progress, hence XHR.
export function uploadDump(file: File, node: string | undefined, onProgress: (f: number) => void, signal?: AbortSignal): Promise<Upload> {
  return new Promise((resolve, reject) => {
    const q = new URLSearchParams({ name: file.name });
    if (node) q.set("node", node);
    const xhr = new XMLHttpRequest();
    xhr.open("POST", `/v1/uploads?${q}`);
    xhr.setRequestHeader("Content-Type", "application/octet-stream");
    xhr.setRequestHeader("X-CSRF-Token", csrf);
    xhr.upload.onprogress = (e) => e.lengthComputable && onProgress(e.loaded / e.total);
    xhr.onload = () => {
      let data: { error?: { code?: string; message?: string } } & Partial<Upload> = {};
      try {
        data = JSON.parse(xhr.responseText);
      } catch {
        /* not JSON */
      }
      if (xhr.status === 201) resolve(data as Upload);
      else reject(new ApiError(xhr.status, data.error?.code ?? "error", data.error?.message ?? xhr.statusText));
    };
    xhr.onerror = () => reject(new ApiError(0, "network", "the upload was interrupted"));
    xhr.onabort = () => reject(new ApiError(0, "aborted", "upload cancelled"));
    signal?.addEventListener("abort", () => xhr.abort());
    xhr.send(file);
  });
}

export type Runtime = { runtime: string; engine: string; version: string };

export type Operation = {
  id: string;
  kind: string;
  target: { resource: string; name: string };
  state: "running" | "succeeded" | "failed";
  node?: string;
  phase?: string;
  error?: string;
};

export type BaseRefresh = {
  base: string;
  engine: string;
  source: { host: string; port: number; user: string; password_set: boolean; databases: string[] };
  every: string;
  updated_at: string;
  last_run: { started_at: string; finished_at: string; ok: boolean; error: string | null } | null;
  next_run: string;
  operation: { id: string; kind: string } | null;
};

export type BaseRefreshInput = {
  source: { host: string; port: number; user: string; password: string; databases: string[] };
  every: string;
};

export type ClusterNode = {
  name: string;
  host: string;
  version: string;
  leader: boolean;
  voter: boolean;
  down: boolean;
  last_seen: string | null;
  pool_size: number;
  pool_free: number;
  arc_max_mb: number;
  arc_problem?: string;
  runtimes: string[];
  protocol: number;
  metrics: Metrics | null;
  /** OS facts of the node; "unknown" for what an older node does not report. */
  system?: NodeSystem;
  /** The node's local db-branch uid/gid, and how they differ from the cluster's (absent: they match). */
  service_ids?: ServiceIds;
  id_problem?: string;
};

export type ServiceIds = { uid: number; gid: number };

export type NodeSystem = { distro: string; os: string; kernel: string; zfs: string };

export type Metrics = { time: string; cpu: number; load1: number; mem_total: number; mem_used: number; net_rx: number; net_tx: number; arc: number };

export type ClusterStatus = { mode: "single" | "cluster"; leader: string; protocol: number; service_ids?: ServiceIds; nodes: ClusterNode[] };

export type UpdateCandidate = { version: string; kind: "minor" | "major"; requires?: string };
export type NodeUpdate = { state: "pending" | "updating" | "done" | "failed"; from?: string; started_at?: string; finished_at?: string; error?: string };
export type Rollout = {
  id: string;
  target: string;
  modules?: Record<string, string>;
  notes?: string[];
  order: string[];
  nodes: Record<string, NodeUpdate>;
  status: "running" | "done" | "failed" | "canceled";
  error?: string;
  started_at: string;
  finished_at?: string;
};
export type UpdateView = {
  checked_at: string;
  checked_by: string;
  nodes: { name: string; leader: boolean; down: boolean; version: string; modules: Record<string, string>; update?: NodeUpdate }[];
  packages: { name: string; installed: string; mixed?: boolean; available: UpdateCandidate[] }[];
  latest_minor: string;
  rollout: Rollout | null;
};

export type Setting = {
  key: string;
  label: string;
  help?: string;
  type: "string" | "text" | "secret" | "int" | "bool" | "duration";
  default: string;
  value: string;
  // translations: a module's own label and help in other languages.
  translations?: Record<string, { label?: string; help?: string }>;
};
export type SettingScope = { scope: string; title: string; settings: Setting[] };
import type { Renewal } from "./lib/license";
export type LicenseReport = {
  license: { id: string; customer: string; modules: string[]; max_nodes: number; expires: string; grace_days: number; plan?: string; billing?: "monthly" | "yearly"; subscription?: string; renew_url?: string };
  state: "valid" | "grace" | "expired" | "over_nodes";
  // trial: no key is saved; the cluster is on (or past) its 30-day trial.
  trial?: boolean;
  all: boolean;
  modules: string[];
  unavailable: string[];
  enabled: string[];
};
export type ModuleState = { requires_license?: boolean; name: string; version: string; wanted: boolean; nodes: { node: string; loaded: string; current: boolean; problem?: string }[] };

export type APIToken = { id: string; name: string; scope: string; created_at: string; expires_at: string | null; token?: string };

export type ModulePage = { module: string; path: string; title: string };
export type Capabilities = { version: string; node: string; modules: string[]; pages: ModulePage[]; record?: boolean };

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
  }
}

let csrf = "";

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET") headers["X-CSRF-Token"] = csrf;
  const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: "same-origin" });
  if (res.status === 204) return undefined as T;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const e = (data as { error?: { code?: string; message?: string } }).error ?? {};
    throw new ApiError(res.status, e.code ?? "error", e.message ?? res.statusText);
  }
  return data as T;
}

export type Session = {
  user: { id: string; name: string; roles: string[] };
  csrf_token: string;
  can_change_password?: boolean;
  language?: string;
  // branch_prefix is put in front of the branch names this user types on the
  // user page ("" for admins). The server adds it; the page only shows it.
  branch_prefix?: string;
  // can: which console actions the user may do (from the server's authorizer).
  can?: Record<string, boolean>;
};

let current: Session | null = null;

// isAdmin tells whether the signed-in user may act for others (owner field,
// everyone's branches).
export const isAdmin = () => !!current?.user.roles.some((r) => r === "admin" || r === "scope:full");

// can tells whether the signed-in user may do action (an older server
// without the list: admins may do everything).
export const can = (action: string) => (current?.can ? !!current.can[action] : isAdmin());

// managesBases: may use the admin console to manage bases (admins and
// maintainers).
// canBase tells whether the caller may do a base action on this base: the
// base's own rights, else the global ones.
export const canBase = (base: { can?: Record<string, boolean> }, action: string) => (base.can && action in base.can ? base.can[action] : can(action));

export const managesBases = () => isAdmin() || can("base.create");

// ssoReturn signs in with the one-time ticket single sign-on leaves in the
// URL (?sso_ticket=), and removes it; it returns the error the provider
// reported (?sso_error=), if any.
export async function ssoReturn(): Promise<{ session?: Session; error?: string }> {
  const q = new URLSearchParams(location.search);
  const ticket = q.get("sso_ticket");
  const error = q.get("sso_error");
  if (!ticket && !error) return {};
  q.delete("sso_ticket");
  q.delete("sso_error");
  history.replaceState(null, "", location.pathname + (q.toString() ? `?${q}` : ""));
  if (error) return { error };
  try {
    return { session: await api.login("sso", ticket!) };
  } catch (e) {
    return { error: (e as Error).message };
  }
}

export type LoginMethod = { module: string; label: string; url: string };

export const api = {
  async session(): Promise<Session> {
    const s = await call<Session>("GET", "/v1/auth/session");
    csrf = s.csrf_token;
    current = s;
    return s;
  },
  async login(username: string, password: string): Promise<Session> {
    const s = await call<Session>("POST", "/v1/auth/login", { username, password });
    csrf = s.csrf_token;
    current = s;
    return s;
  },
  loginMethods: () => call<{ items: LoginMethod[] }>("GET", "/v1/auth/methods").then((r) => r.items),
  logout: () => call<void>("POST", "/v1/auth/logout"),
  changePassword: (current_password: string, new_password: string) =>
    call<void>("PUT", "/v1/auth/password", { current_password, new_password }),

  cluster: () => call<ClusterStatus>("GET", "/v1/cluster"),
  nodeMetrics: (node: string) => call<{ items: Metrics[] }>("GET", `/v1/nodes/${node}/metrics`).then((r) => r.items),
  tokens: () => call<{ items: APIToken[] }>("GET", "/v1/tokens").then((r) => r.items),
  createToken: (t: { name: string; scope: string; ttl?: string }) => call<APIToken>("POST", "/v1/tokens", t),
  revokeToken: (id: string) => call<void>("DELETE", `/v1/tokens/${id}`),
  backups: (node?: string) => call<{ node: string; items: BackupFile[] }>("GET", `/v1/backups${node ? `?node=${encodeURIComponent(node)}` : ""}`),
  takeBackup: (node?: string) => call<BackupFile>("POST", `/v1/backups${node ? `?node=${encodeURIComponent(node)}` : ""}`),
  preferences: () => call<{ language: string; languages: string[] }>("GET", "/v1/account/preferences"),
  setLanguage: (language: string) => call<{ language: string }>("PUT", "/v1/account/preferences", { language }),
  myTokens: () => call<{ items: APIToken[] }>("GET", "/v1/account/tokens").then((r) => r.items),
  createMyToken: (t: { name: string; ttl?: string }) => call<APIToken>("POST", "/v1/account/tokens", t),
  revokeMyToken: (id: string) => call<void>("DELETE", `/v1/account/tokens/${id}`),
  settings: () => call<{ items: SettingScope[] }>("GET", "/v1/settings").then((r) => r.items),
  putSettings: (scope: string, values: Record<string, string>) => call<SettingScope>("PUT", `/v1/settings/${scope}`, { values }),
  license: () => call<{ configured: boolean; report?: LicenseReport; error?: string; renewal?: Renewal }>("GET", "/v1/license"),
  checkLicenseNow: () => call<Renewal>("POST", "/v1/license/renew"),
  verifyLicense: (key: string) => call<LicenseReport>("POST", "/v1/license/verify", { key }),
  applyLicense: (key: string) => call<LicenseReport>("PUT", "/v1/license", { key }),
  updates: () => call<UpdateView>("GET", "/v1/updates"),
  checkUpdates: () => call<UpdateView>("POST", "/v1/updates/check"),
  startUpdate: (target: string) => call<Rollout>("POST", "/v1/updates", { target }),
  cancelUpdate: () => call<void>("DELETE", "/v1/updates"),
  modules: () => call<{ items: ModuleState[]; available: Record<string, string> }>("GET", "/v1/modules"),
  installModule: (name: string) => call<ModuleState>("POST", "/v1/modules", { name }),
  removeModule: (name: string) => call<void>("DELETE", `/v1/modules/${name}`),
  capabilities: () => call<Capabilities>("GET", "/v1/capabilities"),
  branch: (name: string) => call<Branch>("GET", `/v1/branches/${name}`),
  operation: (id: string) => call<Operation>("GET", `/v1/operations/${id}`),
  publicBases: () => call<{ items: PublicBase[]; anonymous: { max_ttl: string; max_branches: number; branches?: number; rate_per_hour?: number } }>("GET", "/v1/public/bases"),
  branches: () => call<{ items: Branch[] }>("GET", "/v1/branches").then((r) => r.items),
  createBranch: (b: { name: string; base: string; ttl?: string; init_sql?: string; owner?: string; access?: string; record?: boolean }) => call<Operation>("POST", "/v1/branches", b),
  recreateBranch: (name: string) => call<Operation>("POST", `/v1/branches/${name}/recreate`),
  resetBranch: (name: string) => call<Operation>("POST", `/v1/branches/${name}/reset`),
  deleteBranch: (name: string) => call<Operation>("DELETE", `/v1/branches/${name}`),
  checkpoints: (name: string) => call<{ items: Checkpoint[] }>("GET", `/v1/branches/${name}/checkpoints`).then((r) => r.items),
  checkpointGraph: (name: string) => call<CheckpointList>("GET", `/v1/branches/${name}/checkpoints`),
  historyEvents: (name: string, line: number, limit = 200) => call<HistoryEvents>("GET", `/v1/ext/history/branches/${name}/events?line=${line}&limit=${limit}`),
  historyRestore: (name: string, to: { line: number; gtid: string }) => call<Operation>("POST", `/v1/ext/history/branches/${name}/restore`, to),
  createCheckpoint: (name: string, checkpoint?: string) => call<Checkpoint>("POST", `/v1/branches/${name}/checkpoints`, { name: checkpoint ?? "" }),
  restoreCheckpoint: (name: string, checkpoint: string) => call<Operation>("POST", `/v1/branches/${name}/checkpoints/${checkpoint}/restore`),
  deleteCheckpoint: (name: string, checkpoint: string) => call<void>("DELETE", `/v1/branches/${name}/checkpoints/${checkpoint}`),
  extendBranch: (name: string, ttl: string) => call<Branch>("PATCH", `/v1/branches/${name}`, { ttl }),
  dsn: (name: string, via?: "proxy") => call<{ dsn: string }>("GET", `/v1/branches/${name}/dsn${via ? "?via=proxy" : ""}`).then((r) => r.dsn),

  bases: () => call<{ items: Base[] }>("GET", "/v1/bases").then((r) => r.items),
  createBase: (b: { name: string; runtime: string; dump?: string; node?: string; description?: string; replica_nodes?: string[] | null }) => call<Operation>("POST", "/v1/bases", b),
  deleteBase: (name: string) => call<Operation>("DELETE", `/v1/bases/${name}`),
  snapshotBase: (name: string) => call<Operation>("POST", `/v1/bases/${name}/snapshots`),
  describeBase: (name: string, description: string) => call<Base>("PATCH", `/v1/bases/${name}`, { description }),
  loadBase: (name: string, dump: string, snapshot: boolean) => call<Operation>("POST", `/v1/bases/${name}/load`, { dump, snapshot }),
  uploadRoom: (node?: string) =>
    call<{ node: string; max_bytes: number; reason: "limit" | "pool" }>("GET", `/v1/uploads/room${node ? `?node=${encodeURIComponent(node)}` : ""}`),
  baseSQL: (name: string, sql: string) => call<void>("POST", `/v1/bases/${name}/sql`, { sql }),
  // setBaseAnonymous changes only the keys given; null inherits the cluster-wide setting.
  setBaseAnonymous: (name: string, b: { max_branches?: number | null; max_ttl?: string | null }) => call<Base>("PATCH", `/v1/bases/${name}/anonymous`, b),
  setSnapshotPublic: (name: string, snapshot: string, pub: boolean) => call<Base>("PATCH", `/v1/bases/${name}/snapshots/${snapshot}`, { public: pub }),
  // baseRefresh is the base's refresh schedule, or null when it has none.
  baseRefresh: (name: string) =>
    call<BaseRefresh>("GET", `/v1/bases/${name}/refresh`).catch((e: unknown) => {
      if (e instanceof ApiError && e.status === 404) return null;
      throw e;
    }),
  setBaseRefresh: (name: string, input: BaseRefreshInput) => call<BaseRefresh>("PUT", `/v1/bases/${name}/refresh`, input),
  clearBaseRefresh: (name: string) => call<void>("DELETE", `/v1/bases/${name}/refresh`),
  runBaseRefresh: (name: string) => call<Operation>("POST", `/v1/bases/${name}/refresh/run`),
  setBaseReplicas: (name: string, replicaNodes: string[] | null) => call<Base>("PUT", `/v1/bases/${name}/replicas`, { replica_nodes: replicaNodes }),
  deleteSnapshot: (name: string, snapshot: string) => call<void>("DELETE", `/v1/bases/${name}/snapshots/${snapshot}`),

  runtimes: () => call<{ items: Runtime[] }>("GET", "/v1/runtimes").then((r) => r.items),
  availableRuntimes: () => call<{ items: Runtime[] }>("GET", "/v1/runtimes/available").then((r) => r.items),
  installRuntime: (runtime: string) => call<Operation>("POST", "/v1/runtimes", { runtime }),
  removeRuntime: (runtime: string) => call<void>("DELETE", `/v1/runtimes/${runtime.replace("@", "/")}`),
  operations: () => call<{ items: Operation[] }>("GET", "/v1/operations").then((r) => r.items),
};
