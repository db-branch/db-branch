// The user page's view of branches: when they expire, what order they come
// in, how far one click extends them, and the two-step confirmation of
// destructive actions. Pure, so it can be tested without a browser.
import { parseDurationMs } from "./duration";
import { t } from "./i18n";

export const ttlChoices: [string, string][] = [
  ["1h", "1 hour"],
  ["4h", "4 hours"],
  ["24h", "1 day"],
  ["72h", "3 days"],
  ["168h", "7 days"],
];

// hours reads a Go-style duration such as "4h", "1h30m", "1.5h", or "30s".
export function hours(d: string): number {
  const ms = parseDurationMs(d);
  return ms === null ? 0 : ms / HOUR;
}

const MIN = 60_000;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;

// SOON is how close to the end a branch is called "expiring soon".
export const SOON = HOUR;
// EXTEND_STEP_MIN is what the one-click Extend adds to the time left.
export const EXTEND_STEP_MIN = 240;

// countdown: "in 3h 12m", "in 2d", "expired" or "no expiry".
export function countdown(expires: string | null, now: number): string {
  if (!expires) return t("no expiry");
  const left = Date.parse(expires) - now;
  if (left <= 0) return t("expired");
  if (left < MIN) return t("in <1m");
  const unit = (big: number, bigName: string, small: number, smallName: string) => {
    const b = Math.floor(left / big);
    const s = Math.floor((left % big) / small);
    return t("in {span}", { span: `${b}${bigName}${s ? ` ${s}${smallName}` : ""}` });
  };
  if (left < HOUR) return t("in {span}", { span: `${Math.floor(left / MIN)}m` });
  if (left < DAY) return unit(HOUR, "h", MIN, "m");
  return unit(DAY, "d", HOUR, "h");
}

export type Expiry = "none" | "ok" | "soon" | "expired";

export function expiryState(expires: string | null, now: number): Expiry {
  if (!expires) return "none";
  const left = Date.parse(expires) - now;
  if (left <= 0) return "expired";
  return left <= SOON ? "soon" : "ok";
}

// sortBranches: `pinned` names first, then the soonest expiry first (expired
// ones lead, branches without expiry come last), then by name.
export function sortBranches<T extends { name: string; expires_at: string | null }>(list: T[], pinned: string[] = []): T[] {
  const end = (b: T) => (b.expires_at ? Date.parse(b.expires_at) : Infinity);
  const rank = (b: T) => {
    const i = pinned.indexOf(b.name);
    return i < 0 ? Infinity : i;
  };
  return [...list].sort((a, b) => {
    if (rank(a) !== rank(b)) return rank(a) < rank(b) ? -1 : 1;
    if (end(a) !== end(b)) return end(a) < end(b) ? -1 : 1;
    return a.name.localeCompare(b.name);
  });
}

// extendTtl: the TTL to send so that the branch lasts `stepMin` minutes longer
// than it does now (the API sets the expiry to now + ttl).
export function extendTtl(expires: string | null, now: number, stepMin = EXTEND_STEP_MIN): string {
  const left = expires ? Math.max(0, Date.parse(expires) - now) : 0;
  return `${Math.ceil(left / MIN) + stepMin}m`;
}

// longerChoices keeps the lifetimes that would end later than the branch does
// now, so choosing one never shortens it.
export function longerChoices(choices: [string, string][], expires: string | null, now: number): [string, string][] {
  const left = expires ? Date.parse(expires) - now : 0;
  return choices.filter(([v]) => hours(v) * HOUR > left);
}

// elapsed: seconds since `since`, short ("7s", "1m 05s").
export function elapsed(since: number, now: number): string {
  const s = Math.max(0, Math.floor((now - since) / 1000));
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, "0")}s`;
}

// isBusy: the branch is being made, reset or changed (not at rest).
export function isBusy(status: string): boolean {
  return !["ready", "failed", "lost", "paused"].includes(status);
}

export type Confirm = null | "reset" | "delete";
export type ConfirmAction = { type: "ask"; what: "reset" | "delete" } | { type: "cancel" } | { type: "timeout" } | { type: "done" };

// confirmReducer: a destructive button first asks; only a second press runs it.
export function confirmReducer(_: Confirm, action: ConfirmAction): Confirm {
  return action.type === "ask" ? action.what : null;
}
