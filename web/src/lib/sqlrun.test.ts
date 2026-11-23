import { describe, expect, it } from "vitest";
import { cellText, isRunKey, resultSummary, splitStatements } from "./sqlrun";

describe("cellText", () => {
  it("shows NULL for a SQL null and the text otherwise", () => {
    expect(cellText(null)).toBe("NULL");
    expect(cellText("")).toBe("");
    expect(cellText("a b")).toBe("a b");
  });
});

describe("resultSummary", () => {
  it("counts the rows and the time", () => {
    expect(resultSummary({ columns: ["a"], rows: [["1"], ["2"]], millis: 12 })).toBe("2 rows · 12 ms");
    expect(resultSummary({ columns: ["a"], rows: [["1"]], millis: 3 })).toBe("1 row · 3 ms");
    expect(resultSummary({ columns: ["a"], rows: [], millis: 0 })).toBe("0 rows · 0 ms");
  });
  it("says how many rows a statement without rows changed", () => {
    expect(resultSummary({ affected: 5, millis: 7 })).toBe("5 rows affected · 7 ms");
    expect(resultSummary({ affected: 1, millis: 7 })).toBe("1 row affected · 7 ms");
  });
  it("says when the rows were cut", () => {
    expect(resultSummary({ columns: ["a"], rows: [["1"]], truncated: true, millis: 5 })).toBe("1 row (cut) · 5 ms");
  });
});

describe("isRunKey", () => {
  it("is Ctrl or Command with Enter", () => {
    expect(isRunKey({ key: "Enter", ctrlKey: true, metaKey: false })).toBe(true);
    expect(isRunKey({ key: "Enter", ctrlKey: false, metaKey: true })).toBe(true);
    expect(isRunKey({ key: "Enter", ctrlKey: false, metaKey: false })).toBe(false);
    expect(isRunKey({ key: "a", ctrlKey: true, metaKey: false })).toBe(false);
  });
});

describe("splitStatements", () => {
  it("keeps one statement as it is, without the closing semicolon, and gives nothing for nothing", () => {
    expect(splitStatements("SELECT 1")).toEqual(["SELECT 1"]);
    expect(splitStatements("  SELECT 1;  ")).toEqual(["SELECT 1"]);
    expect(splitStatements("")).toEqual([]);
    expect(splitStatements(" ; ;\n")).toEqual([]);
  });
  it("splits at the semicolons, over several lines", () => {
    expect(splitStatements("INSERT INTO t VALUES (1);\nINSERT INTO t VALUES (2);\nSELECT * FROM t")).toEqual([
      "INSERT INTO t VALUES (1)",
      "INSERT INTO t VALUES (2)",
      "SELECT * FROM t",
    ]);
  });
  it("does not split inside a quoted string or a quoted name", () => {
    expect(splitStatements("SELECT 'a;b'; SELECT \"c;d\"; SELECT `e;f`")).toEqual(["SELECT 'a;b'", 'SELECT "c;d"', "SELECT `e;f`"]);
    expect(splitStatements("SELECT 'it''s;ok'; SELECT 'back\\'s;x'; SELECT 2")).toEqual(["SELECT 'it''s;ok'", "SELECT 'back\\'s;x'", "SELECT 2"]);
  });
  it("does not split inside a comment, and drops a statement that is only a comment", () => {
    expect(splitStatements("SELECT 1 -- a; b\n; SELECT 2 # c; d\n; SELECT 3 /* e; f */")).toEqual(["SELECT 1 -- a; b", "SELECT 2 # c; d", "SELECT 3 /* e; f */"]);
    expect(splitStatements("-- nothing here;\n/* or here; */")).toEqual([]);
  });
  it("does not split inside a PostgreSQL dollar-quoted body", () => {
    expect(splitStatements("CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql; SELECT f()")).toEqual([
      "CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql",
      "SELECT f()",
    ]);
    expect(splitStatements("SELECT $q$a;b$q$; SELECT 2")).toEqual(["SELECT $q$a;b$q$", "SELECT 2"]);
  });
  it("takes an unterminated string or comment to the end instead of splitting inside it", () => {
    expect(splitStatements("SELECT 'a; b")).toEqual(["SELECT 'a; b"]);
    expect(splitStatements("SELECT 1 /* a; b")).toEqual(["SELECT 1 /* a; b"]);
  });
});

describe("TestSplitStatements_PostgreSQLのJSONBシャープ演算子と末尾バックスラッシュ文字列とネストコメントで誤分割しない", () => {
  it("does not treat PostgreSQL JSONB # operators as comments", () => {
    expect(splitStatements("SELECT data #> '{a;b}' FROM t; SELECT 2")).toEqual(["SELECT data #> '{a;b}' FROM t", "SELECT 2"]);
    expect(splitStatements("SELECT data #>> '{a,b}'; SELECT data #- '{x}'")).toEqual(["SELECT data #>> '{a,b}'", "SELECT data #- '{x}'"]);
    expect(splitStatements("SELECT 5 # 3; SELECT 2", "postgres")).toEqual(["SELECT 5 # 3", "SELECT 2"]);
    expect(splitStatements("SELECT 1 # c; x\n; SELECT 2", "mysql")).toEqual(["SELECT 1 # c; x", "SELECT 2"]);
  });

  it("handles trailing backslash in PostgreSQL string literals and E-strings", () => {
    expect(splitStatements("SELECT 'C:\\'; SELECT 2", "postgres")).toEqual(["SELECT 'C:\\'", "SELECT 2"]);
    expect(splitStatements('SELECT "col\\"; SELECT 2', "postgres")).toEqual(['SELECT "col\\"', "SELECT 2"]);
    expect(splitStatements("SELECT E'a\\'b;c'; SELECT 2", "postgres")).toEqual(["SELECT E'a\\'b;c'", "SELECT 2"]);
    expect(splitStatements("SELECT 'back\\'s;x'; SELECT 2", "mysql")).toEqual(["SELECT 'back\\'s;x'", "SELECT 2"]);
  });

  it("handles nested block comments in PostgreSQL without splitting inside them", () => {
    expect(splitStatements("/* outer /* inner; */ still comment; */ SELECT 1; SELECT 2", "postgres")).toEqual([
      "/* outer /* inner; */ still comment; */ SELECT 1",
      "SELECT 2",
    ]);
    expect(splitStatements("/* outer /* inner; */ still comment; */ SELECT 1; SELECT 2")).toEqual([
      "/* outer /* inner; */ still comment; */ SELECT 1",
      "SELECT 2",
    ]);
    expect(splitStatements("/* only /* nested; */ comment; */", "postgres")).toEqual([]);
    expect(splitStatements("/* a /* b */ SELECT 1; SELECT 2", "mysql")).toEqual(["/* a /* b */ SELECT 1", "SELECT 2"]);
  });
});

