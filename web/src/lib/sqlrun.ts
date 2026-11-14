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

// splitStatements cuts the text at semicolons that end a statement. A semicolon inside a
// quoted string or name, a comment or a PostgreSQL $tag$ body does not. A statement that is
// only a comment is dropped.
export function splitStatements(text: string): string[] {
  const out: string[] = [];
  let start = 0;
  let code = false; // the current statement has something other than comments
  const push = (end: number) => {
    const part = text.slice(start, end).trim();
    if (part && code) out.push(part);
    start = end + 1;
    code = false;
  };
  let i = 0;
  while (i < text.length) {
    const c = text[i];
    const two = text.slice(i, i + 2);
    if (c === "'" || c === '"' || c === "`") {
      code = true;
      i++;
      while (i < text.length) {
        if (text[i] === "\\" && c !== "`") i += 2;
        else if (text[i] === c) {
          if (text[i + 1] === c) i += 2;
          else break;
        } else i++;
      }
      i++;
    } else if (two === "--" || c === "#") {
      const nl = text.indexOf("\n", i);
      i = nl < 0 ? text.length : nl;
    } else if (two === "/*") {
      const end = text.indexOf("*/", i + 2);
      i = end < 0 ? text.length : end + 2;
    } else if (c === "$") {
      const m = /^\$([A-Za-z_][A-Za-z0-9_]*)?\$/.exec(text.slice(i));
      code = true;
      if (m) {
        const end = text.indexOf(m[0], i + m[0].length);
        i = end < 0 ? text.length : end + m[0].length;
      } else i++;
    } else if (c === ";") {
      push(i);
      i++;
    } else {
      if (!/\s/.test(c)) code = true;
      i++;
    }
  }
  push(text.length);
  return out;
}

