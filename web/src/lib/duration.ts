const DURATION_RE = /^(\d+(?:\.\d+)?[hms])+$/;
const PART_RE = /(\d+(?:\.\d+)?)([hms])/g;
const UNIT_MS: Record<"h" | "m" | "s", number> = { h: 3_600_000, m: 60_000, s: 1_000 };

/** Parses a Go-style duration ("4h", "1h30m", "1.5h", "30s") into milliseconds, or null when invalid. */
export function parseDurationMs(input: string): number | null {
  if (!DURATION_RE.test(input)) return null;
  let ms = 0;
  for (const m of input.matchAll(PART_RE)) {
    ms += Number(m[1]) * UNIT_MS[m[2] as "h" | "m" | "s"];
  }
  return Number.isFinite(ms) ? ms : null;
}

/**
 * Parses a positive TTL duration string.
 * When allowDays is true, integer day shorthands like "2d" are normalized to hours ("48h").
 * When caseInsensitive is true, uppercase units such as "2D" or "4H" are lowercased.
 */
export function parsePositiveDuration(input: string, options?: { allowDays?: boolean; caseInsensitive?: boolean }): string | null {
  const raw = input.trim();
  const normalized = options?.caseInsensitive ? raw.toLowerCase() : raw;
  if (options?.allowDays) {
    const d = normalized.match(/^(\d+)\s*d$/);
    if (d) {
      const hours = Number(d[1]) * 24;
      return Number.isFinite(hours) && hours > 0 ? `${hours}h` : null;
    }
  }
  const ms = parseDurationMs(normalized);
  return ms !== null && ms > 0 ? normalized : null;
}
