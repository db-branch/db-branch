// Display helpers. Kept pure so they can be tested without a browser.
import { language, t } from "./i18n";

function localeTag(): string | undefined {
  return language() === "ja" ? "ja-JP" : undefined;
}

function parseDate(iso: string): Date | null {
  if (!iso) return null;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? null : d;
}

export function formatDate(iso: string): string {
  const d = parseDate(iso);
  if (!d) return iso || "—";
  if (language() === "ja") {
    return new Intl.DateTimeFormat("ja-JP", { year: "numeric", month: "2-digit", day: "2-digit" }).format(d);
  }
  return d.toLocaleDateString();
}

export function formatDateTime(iso: string): string {
  const d = parseDate(iso);
  if (!d) return iso || "—";
  if (language() === "ja") {
    return new Intl.DateTimeFormat("ja-JP", {
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hour12: false,
    }).format(d);
  }
  return d.toLocaleString(localeTag());
}

export function formatTime(iso: string): string {
  const d = parseDate(iso);
  if (!d) return iso || "—";
  if (language() === "ja") {
    return new Intl.DateTimeFormat("ja-JP", {
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hour12: false,
    }).format(d);
  }
  return d.toLocaleTimeString(localeTag());
}

export function bytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v >= 10 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

function span(ms: number): string {
  const m = Math.round(ms / 60000);
  if (m < 60) return `${m}m`;
  const h = Math.round(m / 60);
  if (h < 48) return `${h}h`;
  return `${Math.round(h / 24)}d`;
}

export function relative(iso: string, now = Date.now()): string {
  const diff = Date.parse(iso) - now;
  if (Math.abs(diff) < 60000) return t("now");
  const s = span(Math.abs(diff));
  return diff < 0 ? t("{span} ago", { span: s }) : t("in {span}", { span: s });
}

export type TTL = { kind: "none" } | { kind: "expired" } | { kind: "active"; fraction: number; left: string; warn: boolean };

const WARN_MS = 6 * 3600 * 1000;

export function ttlState(created: string, expires: string | null, now = Date.now()): TTL {
  if (!expires) return { kind: "none" };
  const end = Date.parse(expires);
  if (end <= now) return { kind: "expired" };
  const total = end - Date.parse(created);
  const left = end - now;
  return { kind: "active", fraction: total > 0 ? left / total : 1, left: span(left), warn: left < WARN_MS || left / total < 0.15 };
}

export type Shape = "dot" | "ring" | "triangle" | "dashed" | "pause";

export function statusShape(status: string): Shape {
  switch (status) {
    case "ready":
      return "dot";
    case "failed":
      return "triangle";
    case "lost":
      return "dashed";
    case "paused":
      return "pause";
    default:
      return "ring";
  }
}

export function engineLabel(runtime: string): { engine: string; short: string; version: string } {
  const [engine, version] = runtime.split("@");
  return { engine, short: engine === "postgres" ? "Pg" : "My", version };
}

export function storageShare(s: { used: number; referenced: number } | null, max: number): { total: number; own: number } {
  if (!s || max <= 0) return { total: 0, own: 0 };
  return { total: s.referenced / max, own: s.used / max };
}
