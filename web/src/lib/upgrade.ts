// Upgrading a branch to a newer runtime, as the console decides and words it.
// Pure, so it can be tested without a browser; the server
// (domain.CheckUpgradePath) stays the authority.

import { language, t } from "./i18n";

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

// The series of MySQL an upgrade may not jump over (5.7 is one: 5.6 goes to 5.7 before 8.0).
const MYSQL_LTS: Version[] = [
  [5, 7, 0],
  [8, 0, 0],
  [8, 4, 0],
  [9, 7, 0],
];

const series = (v: Version): Version => [v[0], v[1], 0];

// upgradeProblem says why a branch on from cannot move to to ("" when it can).
export function upgradeProblem(from: string, to: string): string {
  const a = parts(from);
  const b = parts(to);
  if (a.engine !== b.engine) return "An upgrade stays within one engine.";
  const c = cmp(a.v, b.v);
  if (c === 0) return "The branch already runs this version.";
  if (c > 0) return "Downgrades are not supported; restore a Checkpoint taken on the older version instead.";
  if (b.engine === "postgres") return "";
  if (a.v[0] < 5 || (a.v[0] === 5 && a.v[1] < 6)) return "Upgrades start from MySQL 5.6.";
  for (const l of MYSQL_LTS) if (cmp(series(a.v), l) < 0 && cmp(l, series(b.v)) < 0) return `This would skip the long-term-support series ${l[0]}.${l[1]}.`;
  return "";
}

// upgradeTargets lists the installed runtimes the branch may move to, oldest first.
export function upgradeTargets(current: string, installed: string[]): string[] {
  return installed
    .filter((r) => upgradeProblem(current, r) === "")
    .sort((x, y) => cmp(parts(x).v, parts(y).v));
}

// upgradeChoices splits the runtimes a branch could move to: the targets already
// installed on the node, and the ones the catalog has that could be installed
// first. Both oldest first.
export function upgradeChoices(current: string, installed: string[], available: string[]): { targets: string[]; toInstall: string[] } {
  const targets = upgradeTargets(current, installed);
  const toInstall = upgradeTargets(current, available).filter((r) => !installed.includes(r));
  return { targets, toInstall };
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
  "pg_upgrade: initdb": "Initializing new cluster",
  "pg_upgrade: checking": "Checking cluster compatibility",
  "pg_upgrade: dumping": "Dumping schemas",
  "pg_upgrade: restoring": "Restoring schemas",
  "pg_upgrade: swapping": "Swapping data directories",
  "pg_upgrade: syncing": "Syncing data directory",
  rolling_back: "Rolling back",
  cloning: "Rolling back",
  swapping: "Rolling back",
  promoting: "Rolling back",
  starting: "Starting the server",
};

export const FINDING_SUMMARIES: Record<string, string> = {
  many_relations: "Many relations: schema dump/restore may take longer.",
  many_large_objects: "Many large objects: pg_upgrade transfers them one by one and may take longer.",
  large_database: "Large database: the Branch becomes ready after pg_upgrade while optimizer statistics update in the background.",
  insufficient_space: "Not enough free pool space for the upgrade.",
  logical_replication: "Logical replication slots or subscriptions exist; verify their state after upgrading.",
  extension_missing: "An installed extension is missing in the target PostgreSQL runtime.",
  extension_update: "An installed extension can be updated with ALTER EXTENSION ... UPDATE after upgrading.",
  pg_upgrade_check_error: "pg_upgrade --check reported an error.",
  pg_upgrade_check_warning: "pg_upgrade --check reported a warning.",
};

export function findingText(f: Finding): { text: string; detail?: string } {
  if (language() !== "ja") return { text: f.message };
  const summary = FINDING_SUMMARIES[f.code];
  if (!summary) return { text: f.message };
  return { text: t(summary), detail: f.message };
}

export type Step =
  | { kind: "closed" }
  | { kind: "choose" }
  | { kind: "confirm"; target: string; force: boolean; warnings?: Finding[] }
  | { kind: "running"; target: string; checkOnly: boolean; phase: string }
  | { kind: "done"; target: string; result?: UpgradeResult; error?: unknown };

export type StepAction =
  | { type: "open" }
  | { type: "close" }
  | { type: "ask"; target: string; force?: boolean; warnings?: Finding[] }
  | { type: "back" }
  | { type: "run"; target: string; checkOnly: boolean }
  | { type: "phase"; phase: string }
  | { type: "finish"; target: string; result?: UpgradeResult; error?: unknown };

// stepReducer: choose a version, confirm, wait, read the result.
export function stepReducer(s: Step, a: StepAction): Step {
  switch (a.type) {
    case "open":
      return s.kind === "closed" || s.kind === "done" ? { kind: "choose" } : s;
    case "close":
      return s.kind === "running" ? s : { kind: "closed" };
    case "ask":
      return s.kind === "choose" || s.kind === "done"
        ? { kind: "confirm", target: a.target, force: !!a.force, ...(a.warnings ? { warnings: a.warnings } : {}) }
        : s;
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
