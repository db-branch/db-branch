import { useState } from "react";
import { api, QueryResult } from "../api";
import { cellText, isRunKey, resultSummary } from "../lib/sqlrun";

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

// SqlRunnerView is the statement box, the Run button and what came back.
export function SqlRunnerView({
  sql,
  onSql,
  onRun,
  running,
  result,
  error,
}: {
  sql: string;
  onSql: (s: string) => void;
  onRun: () => void;
  running: boolean;
  result: QueryResult | null;
  error: string | null;
}) {
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
            if (sql.trim() && !running) onRun();
          }
        }}
      />
      <div className="sql-actions">
        <button type="button" className="primary" disabled={running || !sql.trim()} onClick={onRun}>
          Run
        </button>
        <span className="muted small">Ctrl / ⌘ + Enter</span>
      </div>
      {error && <p className="field-error mono">{error}</p>}
      {result && <SqlResultView result={result} />}
    </div>
  );
}

const MAX_ROWS = 500;

// SqlRunner runs one statement on the branch (its own user; changes stay in the branch).
export function SqlRunner({ name }: { name: string }) {
  const [sql, setSql] = useState("");
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<QueryResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const run = () => {
    setRunning(true);
    api.query(name, sql, MAX_ROWS).then(
      (r) => (setResult(r), setError(null)),
      (e: Error) => (setResult(null), setError(e.message)),
    ).finally(() => setRunning(false));
  };
  return <SqlRunnerView sql={sql} onSql={setSql} onRun={run} running={running} result={result} error={error} />;
}
