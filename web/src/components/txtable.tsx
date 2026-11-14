import type { ReactNode } from "react";
import type { HistoryTx } from "../api";
import { filterTxs, firstLine, rowsLabel, tablesOf, TxFilter, txNumber } from "../lib/history";
import { t } from "../lib/i18n";

// TxTableView lists recorded transactions (newest first) with a search and a
// table filter. The statement opens to its full text. Read only: a restore is
// chosen on the graph.
export function TxTableView({
  items,
  filter,
  onFilter,
  more,
  loading,
  onMore,
  line,
}: {
  items: HistoryTx[];
  filter: TxFilter;
  onFilter: (f: TxFilter) => void;
  more: boolean;
  loading: boolean;
  onMore: () => void;
  line: number;
}): ReactNode {
  const shown = filterTxs(items, filter);
  const tables = tablesOf(items);
  return (
    <div className="tx-table">
      <div className="tx-filter">
        <input type="search" value={filter.text} placeholder={t("Search statements")} aria-label={t("Search statements")} onChange={(e) => onFilter({ ...filter, text: e.target.value })} />
        <select value={filter.table} aria-label={t("Table")} onChange={(e) => onFilter({ ...filter, table: e.target.value })}>
          <option value="">{t("All tables")}</option>
          {tables.map((x) => (
            <option key={x} value={x}>
              {x}
            </option>
          ))}
        </select>
        <span className="muted small">line {line}</span>
      </div>
      {items.length === 0 ? (
        <p className="muted small">{t("No transactions")}</p>
      ) : shown.length === 0 ? (
        <p className="muted small">{t("No match")}</p>
      ) : (
        <div className="hg-table-wrap tx-wrap">
          <table className="hg-table small">
            <thead>
              <tr>
                <th scope="col">#</th>
                <th scope="col">Time</th>
                <th scope="col">Rows</th>
                <th scope="col">Tables</th>
                <th scope="col">Statement</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((x) => (
                <tr key={x.gtid}>
                  <td className="mono">#{txNumber(x.gtid)}</td>
                  <td className="mono muted" title={new Date(x.time).toLocaleString()}>
                    {new Date(x.time).toLocaleTimeString()}
                  </td>
                  <td className="mono">{rowsLabel(x)}</td>
                  <td className="mono small">{x.tables.map((y) => y.name).join(", ")}</td>
                  <td className="mono hg-stmt">
                    {x.sql ? (
                      <details>
                        <summary>{firstLine(x.sql, 100)}</summary>
                        <pre className="hg-sql mono small">{x.sql}</pre>
                        {x.truncated && <span className="muted small">{t("Cut")}</span>}
                      </details>
                    ) : (
                      ""
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {more && (
        <button type="button" onClick={onMore} disabled={loading}>
          {t("Load older")}
        </button>
      )}
    </div>
  );
}
