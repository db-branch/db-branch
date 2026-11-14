// The SQL runner of a branch page: how a result is told. Pure.
import type { QueryResult } from "../api";

// cellText: a value as text; a SQL null is shown as NULL.
export const cellText = (v: string | null): string => (v === null ? "NULL" : v);

// resultSummary is the line under a result: rows (or rows affected) and the time.
export function resultSummary(r: Pick<QueryResult, "rows" | "affected" | "truncated" | "millis" | "columns">): string {
  const time = `${r.millis} ms`;
  if (r.affected !== undefined && !r.rows) return `${r.affected} ${r.affected === 1 ? "row" : "rows"} affected · ${time}`;
  const n = r.rows?.length ?? 0;
  return `${n} ${n === 1 ? "row" : "rows"}${r.truncated ? " (cut)" : ""} · ${time}`;
}

// isRunKey: Ctrl or Command with Enter runs the statement.
export const isRunKey = (e: { key: string; ctrlKey: boolean; metaKey: boolean }): boolean => e.key === "Enter" && (e.ctrlKey || e.metaKey);
