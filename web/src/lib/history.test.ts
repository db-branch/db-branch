import { describe, expect, it } from "vitest";
import { firstLine, hasHistoryModule, rowsLabel, txNumber } from "./history";

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
