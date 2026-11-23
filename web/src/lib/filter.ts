// Search and filters for the lists of branches and bases. Pure.
import { type Base, type Branch, ownerLabel } from "../api";

export type FilterValues = Record<string, string>;

// parseFilter reads the values of keys from a query string ("?q=a&status=failed").
export function parseFilter(search: string, keys: string[]): FilterValues {
  const p = new URLSearchParams(search);
  return Object.fromEntries(keys.map((k) => [k, p.get(k) ?? ""]));
}

// toSearch writes the values that are set as a query string ("" when none is).
export function toSearch(values: FilterValues): string {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(values)) if (v.trim()) p.set(k, v);
  const s = p.toString();
  return s ? `?${s}` : "";
}

// activeCount is how many filters are set.
export function activeCount(values: FilterValues): number {
  return Object.values(values).filter((v) => v.trim() !== "").length;
}

// optionsOf lists the distinct non-empty values, sorted.
export function optionsOf(values: (string | null | undefined)[]): string[] {
  return [...new Set(values.filter((v): v is string => !!v))].sort();
}

export const engineOf = (runtime: string) => runtime.split("@")[0];

// matches: every word of the search text appears somewhere in the haystack.
function matches(text: string, haystack: string[]): boolean {
  const words = text.toLowerCase().split(/\s+/).filter(Boolean);
  const hay = haystack.join(" ").toLowerCase();
  return words.every((w) => hay.includes(w));
}

export function filterBranches(list: Branch[], f: FilterValues): Branch[] {
  return list.filter(
    (b) =>
      (!f.status || b.status === f.status) &&
      (!f.engine || engineOf(b.runtime) === f.engine) &&
      (!f.base || b.base === f.base) &&
      (!f.owner || ownerLabel(b) === f.owner || b.owner === f.owner) &&
      matches(f.q ?? "", [b.name, b.id, b.owner ?? "", b.owner_name ?? "", b.base, b.parent_branch ?? "", b.runtime, b.status, b.status_reason ?? "", b.access]),
  );
}

export function filterBases(list: Base[], f: FilterValues): Base[] {
  return list.filter(
    (b) =>
      (!f.status || b.status === f.status) &&
      (!f.engine || engineOf(b.runtime) === f.engine) &&
      (!f.node || b.node === f.node) &&
      matches(f.q ?? "", [b.name, b.description, b.runtime, b.status, b.status_reason ?? "", b.node]),
  );
}
