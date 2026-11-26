// The SQL runner of a branch page: how a result is told. Pure.
import type { QueryResult } from "../api";
import { errorText } from "./errors";
import { t } from "./i18n";

// cellText: a value as text; a SQL null is shown as NULL.
export const cellText = (v: string | null): string => (v === null ? "NULL" : v);

// resultSummary is the line under a result: rows (or rows affected) and the time.
export function resultSummary(r: Pick<QueryResult, "rows" | "affected" | "truncated" | "millis" | "columns">): string {
  const time = `${r.millis} ms`;
  if (r.affected !== undefined && !r.rows) {
    return t(r.affected === 1 ? "{n} row affected · {time}" : "{n} rows affected · {time}", { n: r.affected, time });
  }
  const n = r.rows?.length ?? 0;
  const cut = r.truncated ? t(" (cut)") : "";
  return t(n === 1 ? "{n} row{cut} · {time}" : "{n} rows{cut} · {time}", { n, cut, time });
}

// isRunKey: Ctrl or Command with Enter runs the statement.
export const isRunKey = (e: { key: string; ctrlKey: boolean; metaKey: boolean }): boolean => e.key === "Enter" && (e.ctrlKey || e.metaKey);

// splitStatements cuts the text at semicolons that end a statement. A semicolon inside a
// quoted string or name, a comment or a PostgreSQL $tag$ body does not. A statement that is
// only a comment is dropped.
export function splitStatements(text: string, engine?: string): string[] {
  const out: string[] = [];
  let start = 0;
  let code = false; // the current statement has something other than comments
  let inCondComment = false;
  const push = (end: number) => {
    const part = text.slice(start, end).trim();
    if (part && code) out.push(part);
    start = end + 1;
    code = false;
    inCondComment = false;
  };
  let i = 0;
  while (i < text.length) {
    const c = text[i];
    const two = text.slice(i, i + 2);
    if (c === "'" || c === '"' || c === "`") {
      code = true;
      const pgEscape = engine === "postgres" && c === "'" && i > 0 && (text[i - 1] === "e" || text[i - 1] === "E") && (i === 1 || !/[A-Za-z0-9_$]/.test(text[i - 2]));
      const backslashEscape = c === "'" ? (engine === "postgres" ? pgEscape : true) : c === '"' ? engine === "mysql" : false;
      i++;
      while (i < text.length) {
        if (backslashEscape && text[i] === "\\") i += 2;
        else if (text[i] === c) {
          if (text[i + 1] === c) i += 2;
          else break;
        } else i++;
      }
      i++;
    } else if (inCondComment && two === "*/") {
      inCondComment = false;
      i += 2;
    } else if (
      (two === "--" && (engine !== "mysql" || i + 2 === text.length || /[\s\x00-\x1f]/.test(text[i + 2]))) ||
      (c === "#" && engine !== "postgres" && (engine === "mysql" || (text[i + 1] !== ">" && text[i + 1] !== "-")))
    ) {
      const nl = text.indexOf("\n", i);
      i = nl < 0 ? text.length : nl;
    } else if (!inCondComment && two === "/*") {
      if (engine === "mysql") {
        if (text.startsWith("/*!", i)) {
          inCondComment = true;
          i += 3;
          for (let d = 0; d < 5 && i < text.length && text[i] >= "0" && text[i] <= "9"; d++) {
            i++;
          }
        } else {
          const end = text.indexOf("*/", i + 2);
          i = end < 0 ? text.length : end + 2;
        }
      } else {
        let depth = 1;
        i += 2;
        while (i < text.length && depth > 0) {
          if (text[i] === "/" && text[i + 1] === "*") {
            depth++;
            i += 2;
          } else if (text[i] === "*" && text[i + 1] === "/") {
            depth--;
            i += 2;
          } else {
            i++;
          }
        }
      }
    } else if (c === "$" && engine !== "mysql") {
      const m = /^\$([A-Za-z_][A-Za-z0-9_]*)?\$/.exec(text.slice(i));
      code = true;
      if (m) {
        const end = text.indexOf(m[0], i + m[0].length);
        i = end < 0 ? text.length : end + m[0].length;
      } else i++;
    } else if (c === ";" && !inCondComment) {
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

export type SqlRunItem = { sql: string; result?: QueryResult; error?: string };

export type SqlRunnerState = {
  running: boolean;
  stopped: boolean;
  items: SqlRunItem[];
  total: number;
};

// runStatements runs statements sequentially until all finish, one errors, or signal aborts.
export async function runStatements(
  statements: string[],
  exec: (sql: string, signal: AbortSignal) => Promise<QueryResult>,
  signal: AbortSignal,
  onProgress?: (items: SqlRunItem[]) => void,
): Promise<{ items: SqlRunItem[]; stopped: boolean }> {
  const done: SqlRunItem[] = [];
  for (const st of statements) {
    if (signal.aborted) {
      return { items: done, stopped: true };
    }
    try {
      const result = await exec(st, signal);
      done.push({ sql: st, result });
      onProgress?.([...done]);
      if (signal.aborted) {
        return { items: done, stopped: done.length < statements.length };
      }
    } catch (e) {
      if (signal.aborted || (e instanceof Error && e.name === "AbortError")) {
        return { items: done, stopped: true };
      }
      done.push({ sql: st, error: errorText(e) });
      onProgress?.([...done]);
      return { items: done, stopped: false };
    }
  }
  return { items: done, stopped: false };
}

// createSqlRunnerController manages an abortable sequential SQL run and component unmount cleanup.
export function createSqlRunnerController(
  exec: (sql: string, database: string | undefined, signal: AbortSignal) => Promise<QueryResult>,
  onState: (state: SqlRunnerState) => void,
  engine?: string,
) {
  let disposed = false;
  let controller: AbortController | null = null;
  let state: SqlRunnerState = { running: false, stopped: false, items: [], total: 0 };
  const emit = (next: SqlRunnerState) => {
    state = next;
    if (!disposed) onState(next);
  };
  return {
    async run(sql: string, database?: string) {
      if (state.running || disposed) return;
      const list = splitStatements(sql, engine);
      if (list.length === 0) return;
      const ac = new AbortController();
      controller = ac;
      emit({ running: true, stopped: false, items: [], total: list.length });
      const db = database?.trim() || undefined;
      const res = await runStatements(
        list,
        (st, signal) => exec(st, db, signal),
        ac.signal,
        (items) => emit({ running: true, stopped: false, items, total: list.length }),
      );
      if (controller === ac) controller = null;
      emit({ running: false, stopped: res.stopped, items: res.items, total: list.length });
    },
    stop() {
      controller?.abort();
    },
    dispose() {
      disposed = true;
      controller?.abort();
      controller = null;
    },
  };
}

