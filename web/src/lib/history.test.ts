import { describe, expect, it } from "vitest";
import type { HistoryTx } from "../api";
import { filterTxs, firstLine, hasHistoryModule, rowsLabel, tablesOf, txNumber } from "./history";

describe("hasHistoryModule", () => {
  it("is true only when the history module is loaded", () => {
    expect(hasHistoryModule({ modules: ["proxysql 0.1.0", "history 0.2.0"] })).toBe(true);
    expect(hasHistoryModule({ modules: ["history"] })).toBe(true);
    expect(hasHistoryModule({ modules: ["proxysql 0.1.0", "history-extra 1"] })).toBe(false);
    expect(hasHistoryModule({ modules: [] })).toBe(false);
    expect(hasHistoryModule(undefined)).toBe(false);
  });
});

describe("txNumber", () => {
  it("is the sequence number of the GTID", () => {
    expect(txNumber("bcbb7a9f-de9f-11f1-b6b1-bc24113d770e:42")).toBe("42");
    expect(txNumber("plain")).toBe("plain");
  });
});

describe("rowsLabel", () => {
  it("counts what changed and leaves out what did not", () => {
    expect(rowsLabel({ inserted: 2, updated: 1, deleted: 0, ddl: false })).toBe("+2 ~1");
    expect(rowsLabel({ inserted: 0, updated: 0, deleted: 5, ddl: false })).toBe("-5");
    expect(rowsLabel({ inserted: 0, updated: 0, deleted: 0, ddl: true })).toBe("schema");
    expect(rowsLabel({ inserted: 0, updated: 0, deleted: 0, ddl: false })).toBe("no rows");
  });
});

describe("firstLine", () => {
  it("joins lines and cuts long text", () => {
    expect(firstLine("insert into t\n   values (1)")).toBe("insert into t values (1)");
    expect(firstLine("x".repeat(100), 10)).toBe("xxxxxxxxx…");
  });
});

const mk = (n: number, sql: string, tables: string[], extra: Partial<HistoryTx> = {}): HistoryTx => ({
  gtid: `u:${n}`,
  time: "2026-11-12T10:00:00Z",
  sql,
  truncated: false,
  tables: tables.map((name) => ({ name, inserted: 1, updated: 0, deleted: 0 })),
  inserted: 1,
  updated: 0,
  deleted: 0,
  ddl: false,
  file: "binlog.000001",
  ...extra,
});
const sample = [mk(1, "INSERT INTO shop.orders VALUES (1)", ["shop.orders"]), mk(2, "UPDATE shop.items SET qty = 2", ["shop.items"]), mk(3, "DELETE FROM shop.orders WHERE id = 1", ["shop.orders"]), mk(4, "", ["shop.items"])];

describe("tablesOf", () => {
  it("lists the tables the transactions touched, once each, sorted", () => {
    expect(tablesOf(sample)).toEqual(["shop.items", "shop.orders"]);
    expect(tablesOf([])).toEqual([]);
  });
});

describe("filterTxs", () => {
  it("keeps everything for an empty filter", () => {
    expect(filterTxs(sample, { text: "", table: "" })).toHaveLength(4);
    expect(filterTxs(sample, { text: "  ", table: "" })).toHaveLength(4);
  });
  it("matches the statement without regard to case, and the transaction number", () => {
    expect(filterTxs(sample, { text: "update", table: "" }).map((x) => x.gtid)).toEqual(["u:2"]);
    expect(filterTxs(sample, { text: "#3", table: "" }).map((x) => x.gtid)).toEqual(["u:3"]);
    expect(filterTxs(sample, { text: "3", table: "" }).map((x) => x.gtid)).toEqual(["u:3"]);
  });
  it("narrows by table, and both filters together", () => {
    expect(filterTxs(sample, { text: "", table: "shop.orders" }).map((x) => x.gtid)).toEqual(["u:1", "u:3"]);
    expect(filterTxs(sample, { text: "delete", table: "shop.orders" }).map((x) => x.gtid)).toEqual(["u:3"]);
    expect(filterTxs(sample, { text: "delete", table: "shop.items" })).toEqual([]);
  });
});
