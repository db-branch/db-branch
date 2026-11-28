// Limits on branches made without signing in, per base. Pure helpers for the
// Bases page so they can be tested without a browser.
import { parsePositiveDuration } from "./duration";

// What a base allows besides the cluster-wide settings; null inherits them.
export type AnonLimits = { max_branches: number | null; max_ttl: string | null };

export type Patch = { max_branches?: number | null; max_ttl?: string | null };

// shortDuration turns the server's "4h0m0s" into "4h".
export function shortDuration(s: string): string {
  return s.replace(/(?<=[hm])0[ms]$/, "").replace(/(?<=h)0m$/, "");
}

const INHERIT = (s: string) => s.trim() === "" || s.trim().toLowerCase() === "inherit";

export type Parsed<T> = { ok: true; value: T | null } | { ok: false };

// parseMax reads the "max branches" box: empty or "inherit" inherits, else a whole number from 0.
export function parseMax(input: string): Parsed<number> {
  if (INHERIT(input)) return { ok: true, value: null };
  const s = input.trim();
  return /^\d+$/.test(s) ? { ok: true, value: Number(s) } : { ok: false };
}

// parseTTL reads the "max TTL" box: empty or "inherit" inherits, else a positive duration such as 4h or 1h30m.
export function parseTTL(input: string): Parsed<string> {
  if (INHERIT(input)) return { ok: true, value: null };
  const ttl = parsePositiveDuration(input);
  return ttl !== null ? { ok: true, value: ttl } : { ok: false };
}

// inputsOf is what the two boxes show for the current settings.
export function inputsOf(l: AnonLimits): { max: string; ttl: string } {
  return { max: l.max_branches === null ? "" : String(l.max_branches), ttl: l.max_ttl === null ? "" : shortDuration(l.max_ttl) };
}

// changes builds the request for what was typed, with only the boxes that differ from the current settings.
export function changes(current: AnonLimits, maxInput: string, ttlInput: string): { ok: true; body: Patch } | { ok: false; field: "max" | "ttl" } {
  const max = parseMax(maxInput);
  if (!max.ok) return { ok: false, field: "max" };
  const ttl = parseTTL(ttlInput);
  if (!ttl.ok) return { ok: false, field: "ttl" };
  const body: Patch = {};
  if (max.value !== current.max_branches) body.max_branches = max.value;
  const was = current.max_ttl === null ? null : shortDuration(current.max_ttl);
  if (ttl.value !== was) body.max_ttl = ttl.value;
  return { ok: true, body };
}

// anonymousCount is how many branches made without signing in exist from the base.
export function anonymousCount(branches: { owner: string | null; base: string }[] | undefined, base: string): number {
  return (branches ?? []).filter((b) => b.owner === "anonymous" && b.base === base).length;
}
