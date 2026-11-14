import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { HistoryTx } from "../api";
import { TxTableView } from "./txtable";

const mk = (n: number, sql: string, table = "shop.orders", extra: Partial<HistoryTx> = {}): HistoryTx => ({
  gtid: `u:${n}`,
  time: "2026-11-12T10:00:00Z",
  sql,
  truncated: false,
  tables: [{ name: table, inserted: 1, updated: 0, deleted: 0 }],
  inserted: 1,
  updated: 0,
  deleted: 0,
  ddl: false,
  file: "f",
  ...extra,
});
const noop = () => {};
const view = (items: HistoryTx[], props: Record<string, unknown> = {}) =>
  renderToStaticMarkup(<TxTableView items={items} filter={{ text: "", table: "" }} onFilter={noop} more={false} loading={false} onMore={noop} line={3} {...props} />);

describe("TxTableView", () => {
  it("lists the transactions newest first as a table with time, number, rows, tables and statement", () => {
    const h = view([mk(2, "UPDATE a SET b = 1"), mk(1, "INSERT INTO a VALUES (1)")]);
    expect(h).toMatch(/<table/);
    for (const c of ["Time", "Rows", "Tables", "Statement"]) expect(h).toContain(c);
    expect(h.indexOf("UPDATE a")).toBeLessThan(h.indexOf("INSERT INTO a"));
    expect(h).toContain("#2");
    expect(h).toContain("shop.orders");
  });
  it("shows the whole statement when it is opened, and marks one that was cut", () => {
    const h = view([mk(1, "INSERT INTO a VALUES (1)\nSELECT 2", "t", { truncated: true })]);
    expect(h).toContain("<details");
    expect(h).toContain("INSERT INTO a VALUES (1)");
    expect(h).toContain("Cut");
  });
  it("filters the rows it is given by the text and the table", () => {
    const items = [mk(1, "INSERT one"), mk(2, "INSERT two", "shop.items")];
    expect(view(items, { filter: { text: "two", table: "" } })).not.toContain("INSERT one");
    expect(view(items, { filter: { text: "", table: "shop.items" } })).not.toContain("INSERT one");
    const h = view(items, { filter: { text: "zzz", table: "" } });
    expect(h).toContain("No match");
  });
  it("offers the tables to choose from, and loading older ones only when there are more", () => {
    const items = [mk(1, "x"), mk(2, "y", "shop.items")];
    const h = view(items);
    expect(h).toContain("<option");
    expect(h).toContain("shop.items");
    expect(h).not.toContain("Load older");
    expect(view(items, { more: true })).toContain("Load older");
    expect(view(items, { more: true, loading: true })).toMatch(/<button[^>]*disabled[^>]*>Load older/);
  });
  it("says so when nothing is recorded yet", () => {
    expect(view([])).toContain("No transactions");
  });
});
