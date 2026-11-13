// Upgrading a branch to a newer runtime, as the console decides and words it.
// Pure, so it can be tested without a browser; the server
// (domain.CheckUpgradePath) stays the authority.

export type Level = "error" | "warning" | "info";
export type Finding = { level: Level; code: string; message: string };

export type UpgradeStatus = "checked" | "upgraded" | "rolled_back" | "failed" | "refused" | "aborted";

// UpgradeResult is the result of a branch.upgrade operation (also when it failed).
export type UpgradeResult = {
  branch: string;
  from: string;
  to: string;
  status: UpgradeStatus;
  check_only: boolean;
  checkpoint?: string;
  pre_check: Finding[];
  log: Finding[];
  error?: string;
  log_excerpt?: string;
};

type Version = [number, number, number];

function parts(runtime: string): { engine: string; v: Version } {
  const [engine, version = ""] = runtime.split("@");
  const nums = version.split("-")[0].split(".").map((n) => parseInt(n, 10) || 0);
  return { engine, v: [nums[0] ?? 0, nums[1] ?? 0, nums[2] ?? 0] };
}

function cmp(a: Version, b: Version): number {
  for (let i = 0; i < 3; i++) if (a[i] !== b[i]) return a[i] < b[i] ? -1 : 1;
  return 0;
}

// The long-term-support series of MySQL; an upgrade may not jump over one.
const MYSQL_LTS: Version[] = [
  [8, 0, 0],
  [8, 4, 0],
  [9, 7, 0],
];

const series = (v: Version): Version => [v[0], v[1], 0];
const pgMajor = (v: Version): Version => (v[0] >= 10 ? [v[0], 0, 0] : [v[0], v[1], 0]);

// upgradeProblem says why a branch on from cannot move to to ("" when it can).
export function upgradeProblem(from: string, to: string): string {
  const a = parts(from);
  const b = parts(to);
  if (a.engine !== b.engine) return "An upgrade stays within one engine.";
  const c = cmp(a.v, b.v);
  if (c === 0) return "The branch already runs this version.";
  if (c > 0) return "Downgrades are not supported; restore a Checkpoint taken on the older version instead.";
  if (b.engine === "postgres") return cmp(pgMajor(a.v), pgMajor(b.v)) === 0 ? "" : "PostgreSQL major version upgrades are not supported yet.";
  if (a.v[0] < 8) return "Upgrades start from MySQL 8.0.";
  for (const l of MYSQL_LTS) if (cmp(series(a.v), l) < 0 && cmp(l, series(b.v)) < 0) return `This would skip the long-term-support series ${l[0]}.${l[1]}.`;
  return "";
}

// upgradeTargets lists the installed runtimes the branch may move to, oldest first.
export function upgradeTargets(current: string, installed: string[]): string[] {
  return installed
    .filter((r) => upgradeProblem(current, r) === "")
    .sort((x, y) => cmp(parts(x).v, parts(y).v));
}

// installHint is the command that installs a runtime.
export function installHint(runtime: string): string {
  return `db-branch runtime install ${runtime}`;
}

// isUpgraded: the branch runs another runtime than its base.
export function isUpgraded(runtime: string, baseRuntime?: string): boolean {
  return !!baseRuntime && baseRuntime !== runtime;
}

export type Tone = "good" | "warn" | "bad";

// tone is how a finished upgrade is coloured.
export function resultTone(r: UpgradeResult): Tone {
  switch (r.status) {
    case "upgraded":
      return r.log.some((f) => f.level === "error") ? "warn" : "good";
    case "checked":
      return r.pre_check.some((f) => f.level === "error") ? "bad" : r.pre_check.some((f) => f.level === "warning") ? "warn" : "good";
    default:
      return "bad";
  }
}

// count of the findings of one level.
export function countLevel(list: Finding[], level: Level): number {
  return list.filter((f) => f.level === level).length;
}

// canForce: the pre-check refused the upgrade for errors the person may override.
export function canForce(r: UpgradeResult): boolean {
  return r.status === "refused" || (r.status === "checked" && countLevel(r.pre_check, "error") > 0);
}

// Where the operation's own phases are named for people.
export const PHASES: Record<string, string> = {
  checking: "Checking",
  stopping: "Stopping the server",
  checkpointing: "Saving a Checkpoint",
  upgrading: "Starting the new version",
  rolling_back: "Rolling back",
  cloning: "Rolling back",
  swapping: "Rolling back",
  promoting: "Rolling back",
  starting: "Starting the server",
};

export type Step =
  | { kind: "closed" }
  | { kind: "choose" }
  | { kind: "confirm"; target: string; force: boolean }
  | { kind: "running"; target: string; checkOnly: boolean; phase: string }
  | { kind: "done"; target: string; result?: UpgradeResult; error?: string };

export type StepAction =
  | { type: "open" }
  | { type: "close" }
  | { type: "ask"; target: string; force?: boolean }
  | { type: "back" }
  | { type: "run"; target: string; checkOnly: boolean }
  | { type: "phase"; phase: string }
  | { type: "finish"; target: string; result?: UpgradeResult; error?: string };

// stepReducer: choose a version, confirm, wait, read the result.
export function stepReducer(s: Step, a: StepAction): Step {
  switch (a.type) {
    case "open":
      return s.kind === "closed" || s.kind === "done" ? { kind: "choose" } : s;
    case "close":
      return s.kind === "running" ? s : { kind: "closed" };
    case "ask":
      return s.kind === "choose" || s.kind === "done" ? { kind: "confirm", target: a.target, force: !!a.force } : s;
    case "back":
      return s.kind === "confirm" ? { kind: "choose" } : s;
    case "run":
      return s.kind === "choose" || s.kind === "confirm" || s.kind === "done" ? { kind: "running", target: a.target, checkOnly: a.checkOnly, phase: "" } : s;
    case "phase":
      return s.kind === "running" ? { ...s, phase: a.phase } : s;
    case "finish":
      return s.kind === "running" ? { kind: "done", target: a.target, result: a.result, error: a.error } : s;
  }
}
