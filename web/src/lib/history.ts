// Helpers for the paid history module as the console shows it. Pure.
import type { Capabilities, HistoryTx } from "../api";

// hasHistoryModule: the history module is loaded (capabilities list "name version").
export function hasHistoryModule(caps: Pick<Capabilities, "modules"> | undefined): boolean {
  return !!caps?.modules.some((m) => m === "history" || m.startsWith("history "));
}

// txNumber: the transaction's sequence number in its GTID ("uuid:42" is 42).
export function txNumber(gtid: string): string {
  return gtid.split(":").pop() ?? gtid;
}

// rowsLabel: "+2 ~1 -0" style counts, leaving out what is zero.
export function rowsLabel(tx: Pick<HistoryTx, "inserted" | "updated" | "deleted" | "ddl">): string {
  if (tx.ddl && tx.inserted + tx.updated + tx.deleted === 0) return "schema";
  return [tx.inserted && `+${tx.inserted}`, tx.updated && `~${tx.updated}`, tx.deleted && `-${tx.deleted}`].filter(Boolean).join(" ") || "no rows";
}

// firstLine: the statement shortened to one line for a list or a tooltip.
export function firstLine(sql: string, max = 90): string {
  const one = sql.replace(/\s+/g, " ").trim();
  return one.length > max ? one.slice(0, max - 1) + "…" : one;
}

export type TxFilter = { text: string; table: string };

// tablesOf: the tables the transactions touched, once each, sorted.
export function tablesOf(items: HistoryTx[]): string[] {
  return [...new Set(items.flatMap((x) => x.tables.map((y) => y.name)))].sort();
}

// filterTxs keeps the transactions that match the search text (in the statement,
// or the transaction number, with or without "#") and touch the table.
export function filterTxs(items: HistoryTx[], f: TxFilter): HistoryTx[] {
  const q = f.text.trim().toLowerCase().replace(/^#/, "");
  return items.filter((x) => (!f.table || x.tables.some((y) => y.name === f.table)) && (!q || x.sql.toLowerCase().includes(q) || txNumber(x.gtid) === q));
}

