import { useEffect, useMemo, useState } from "react";
import { api, QueryResult } from "../api";
import { t } from "../lib/i18n";
import { cellText, createSqlRunnerController, isRunKey, resultSummary, splitStatements, type SqlRunnerState, type SqlRunItem } from "../lib/sqlrun";

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

export type SqlItem = SqlRunItem;

// SqlRunnerView is the statements box, the Run button and what came back for each statement.
export function SqlRunnerView({
  sql,
  onSql,
  engine,
  database,
  onDatabase,
  onRun,
  onStop,
  running,
  stopped,
  items,
  total,
}: {
  sql: string;
  onSql: (s: string) => void;
  engine?: string;
  database?: string;
  onDatabase?: (db: string) => void;
  onRun: () => void;
  onStop?: () => void;
  running: boolean;
  stopped?: boolean;
  items: SqlItem[];
  total?: number;
}) {
  const empty = splitStatements(sql, engine).length === 0;
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
        {running && onStop && (
          <button type="button" onClick={onStop}>
            Stop
          </button>
        )}
        {onDatabase && (
          <input
            type="text"
            className="mono sql-db"
            aria-label="Database"
            placeholder="database"
            value={database ?? ""}
            disabled={running}
            onChange={(e) => onDatabase(e.target.value)}
          />
        )}
        <span className="muted small">Ctrl / ⌘ + Enter</span>
        {running && total ? <span className="muted small mono">{`${items.length} / ${total}`}</span> : null}
      </div>
      {stopped && total ? (
        <p className="muted small">
          {t("Stopped.")} <span className="mono">{`(${items.length} / ${total})`}</span>
        </p>
      ) : null}
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
// in the branch). It stops at the first one that fails, on Stop, or on unmount.
export function SqlRunner({ name, engine }: { name: string; engine?: string }) {
  const [sql, setSql] = useState("");
  const [database, setDatabase] = useState("");
  const [state, setState] = useState<SqlRunnerState>({ running: false, stopped: false, items: [], total: 0 });
  const ctrl = useMemo(
    () => createSqlRunnerController((st, db, signal) => api.query(name, st, MAX_ROWS, db, signal), setState, engine),
    [name, engine],
  );
  useEffect(() => () => ctrl.dispose(), [ctrl]);
  return (
    <SqlRunnerView
      sql={sql}
      onSql={setSql}
      engine={engine}
      database={database}
      onDatabase={setDatabase}
      onRun={() => void ctrl.run(sql, database)}
      onStop={() => ctrl.stop()}
      running={state.running}
      stopped={state.stopped}
      items={state.items}
      total={state.total}
    />
  );
}
