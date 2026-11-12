// Choosing a base among many: search, engine filter, order, and the
// bases this person used last. Pure so it can be tested without a browser.

export type Choice = {
  name: string;
  runtime: string;
  description: string;
  snapshot: string;
  snapshotAt: string | null;
  public: boolean;
  masked: boolean;
};

export type EngineFilter = "all" | "mysql" | "postgres";

export function engineOf(runtime: string): string {
  return runtime.split("@")[0];
}

const ALIASES: Record<string, string> = { postgres: "postgres postgresql pg", mysql: "mysql" };

// matches: every word of the query must appear (in any order) in the name,
// description, engine or version; the engine filter narrows further.
export function matches(c: Choice, query: string, engine: EngineFilter): boolean {
  const e = engineOf(c.runtime);
  if (engine !== "all" && e !== engine) return false;
  const hay = `${c.name} ${c.description} ${c.runtime.replace("@", " ")} ${ALIASES[e] ?? e}`.toLowerCase();
  return query
    .toLowerCase()
    .split(/\s+/)
    .filter(Boolean)
    .every((w) => hay.includes(w));
}

// arrange: the bases used lately first (latest first), then by name.
export function arrange(choices: Choice[], recent: string[]): Choice[] {
  const rank = (n: string) => {
    const i = recent.indexOf(n);
    return i < 0 ? Infinity : i;
  };
  return [...choices].sort((a, b) => {
    const d = rank(a.name) - rank(b.name);
    if (d !== 0 && !(Number.isNaN(d))) return d < 0 ? -1 : 1;
    return a.name.localeCompare(b.name);
  });
}

export const RECENT_MAX = 8;

export function pushRecent(recent: string[], name: string): string[] {
  return [name, ...recent.filter((n) => n !== name)].slice(0, RECENT_MAX);
}

const KEY = "db-branch.recent-bases";

export function loadRecent(): string[] {
  try {
    const v = JSON.parse(localStorage.getItem(KEY) ?? "[]");
    return Array.isArray(v) ? v.filter((x) => typeof x === "string") : [];
  } catch {
    return [];
  }
}

export function saveRecent(recent: string[]) {
  try {
    localStorage.setItem(KEY, JSON.stringify(recent));
  } catch {
    /* storage unavailable: the order lasts this visit */
  }
}

// initialBase: the last used base if it is still offered, else the first.
export function initialBase(arranged: Choice[], recent: string[]): string {
  return (arranged.find((c) => recent.includes(c.name)) ?? arranged[0]).name;
}

// moveSelection: arrow keys over the visible list, wrapping nowhere.
export function moveSelection(names: string[], current: string, delta: number): string {
  if (names.length === 0) return current;
  const i = names.indexOf(current);
  const next = Math.min(names.length - 1, Math.max(0, (i < 0 ? 0 : i) + delta));
  return names[next];
}
