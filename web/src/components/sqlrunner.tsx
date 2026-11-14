import { useState } from "react";
import { api, QueryResult } from "../api";
import { cellText, isRunKey, resultSummary, splitStatements } from "../lib/sqlrun";

// SqlResultView draws a result: the rows as a table, or how many rows a statement changed.
export function SqlResultView({ result: r }: { result: QueryResult }) {
  return (
    <div className="sql-result">
      {r.columns && r.rows && (
        <div className="sql-table-wrap">
          <table className="rows compact sql-table">
            <thead>
              <tr>
                {r.columns.map((c, i) => (
                  <th key={i}>{c}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {r.rows.map((row, i) => (
                <tr key={i}>
                  {row.map((v, j) => (
                    <td key={j} className={v === null ? "mono muted null" : "mono"}>
                      {cellText(v)}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <p className="muted small mono">{resultSummary(r)}</p>
    </div>
  );
}

export type SqlItem = { sql: string; result?: QueryResult; error?: string };

// SqlRunnerView is the statements box, the Run button and what came back for each statement.
export function SqlRunnerView({
  sql,
  onSql,
  onRun,
  running,
  items,
  total,
}: {
  sql: string;
  onSql: (s: string) => void;
  onRun: () => void;
  running: boolean;
  items: SqlItem[];
  total?: number;
}) {
  const empty = splitStatements(sql).length === 0;
  const many = items.length > 1;
  return (
    <div className="sql-runner">
      <textarea
        className="mono"
        rows={5}
        spellCheck={false}
        aria-label="SQL"
        value={sql}
        placeholder="SELECT …"
        onChange={(e) => onSql(e.target.value)}
        onKeyDown={(e) => {
          if (isRunKey(e)) {
            e.preventDefault();
            if (!empty && !running) onRun();
          }
        }}
      />
      <div className="sql-actions">
        <button type="button" className="primary" disabled={running || empty} onClick={onRun}>
          Run
        </button>
        <span className="muted small">Ctrl / ⌘ + Enter</span>
        {running && total ? <span className="muted small mono">{`${items.length} / ${total}`}</span> : null}
      </div>
      {items.map((it, i) => (
        <div key={i} className={many ? "sql-stmt" : undefined}>
          {many && <pre className="mono small sql-stmt-text">{it.sql}</pre>}
          {it.error && <p className="field-error mono">{it.error}</p>}
          {it.result && <SqlResultView result={it.result} />}
        </div>
      ))}
    </div>
  );
}

const MAX_ROWS = 500;

// SqlRunner runs the statements one after another on the branch (its own user; changes stay
// in the branch). It stops at the first one that fails.
export function SqlRunner({ name }: { name: string }) {
  const [sql, setSql] = useState("");
  const [running, setRunning] = useState(false);
  const [items, setItems] = useState<SqlItem[]>([]);
  const [total, setTotal] = useState(0);
  const run = async () => {
    const list = splitStatements(sql);
    setRunning(true);
    setTotal(list.length);
    setItems([]);
    const done: SqlItem[] = [];
    for (const st of list) {
      try {
        done.push({ sql: st, result: await api.query(name, st, MAX_ROWS) });
      } catch (e) {
        done.push({ sql: st, error: (e as Error).message });
        setItems([...done]);
        break;
      }
      setItems([...done]);
    }
    setRunning(false);
  };
  return <SqlRunnerView sql={sql} onSql={setSql} onRun={run} running={running} items={items} total={total} />;
}
