import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { SqlResultView, SqlRunnerView } from "./sqlrunner";

const noop = () => {};
const run = (props: Record<string, unknown> = {}) =>
  renderToStaticMarkup(<SqlRunnerView sql="SELECT 1" onSql={noop} onRun={noop} running={false} items={[]} {...props} />);

describe("SqlRunnerView", () => {
  it("has a box for the statements and a Run button, and shows the shortcut", () => {
    const h = run();
    expect(h).toMatch(/<textarea[^>]*aria-label="SQL"/);
    expect(h).toContain("SELECT 1");
    expect(h).toMatch(/<button[^>]*>Run</);
    expect(h).toMatch(/Ctrl|⌘/);
  });
  it("disables Run while it runs or the box is empty", () => {
    expect(run({ running: true })).toMatch(/<button[^>]*disabled[^>]*>Run/);
    expect(run({ sql: "  " })).toMatch(/<button[^>]*disabled[^>]*>Run/);
    expect(run({ sql: " ; " })).toMatch(/<button[^>]*disabled[^>]*>Run/);
  });
  it("shows the error of a failed statement", () => {
    expect(run({ items: [{ sql: "SELEC 1", error: "You have an error in your SQL syntax" }] })).toContain("You have an error in your SQL syntax");
  });
  it("shows one result without a heading, and several with the statement above each", () => {
    const one = run({ items: [{ sql: "SELECT 1", result: { columns: ["1"], rows: [["1"]], millis: 1 } }] });
    expect(one).not.toContain("sql-stmt");
    const many = run({
      items: [
        { sql: "INSERT INTO t VALUES (1)", result: { affected: 1, millis: 2 } },
        { sql: "SELECT * FROM t", result: { columns: ["id"], rows: [["1"]], millis: 3 } },
        { sql: "SELEC oops", error: "syntax error" },
      ],
    });
    expect(many.match(/class="sql-stmt/g)).toHaveLength(3);
    expect(many).toContain("INSERT INTO t VALUES (1)");
    expect(many.indexOf("INSERT INTO t")).toBeLessThan(many.indexOf("SELECT * FROM t"));
    expect(many).toContain("syntax error");
  });
  it("tells the number of statements while it runs", () => {
    expect(run({ running: true, total: 3, items: [{ sql: "a", result: { affected: 1, millis: 1 } }] })).toContain("1 / 3");
  });
});

describe("SqlResultView", () => {
  const res = { columns: ["id", "name"], rows: [["1", "alice"], ["2", null]], millis: 4 };
  it("draws the rows as a table with the columns, NULL marked, and the summary", () => {
    const h = renderToStaticMarkup(<SqlResultView result={res} />);
    expect(h).toMatch(/<table/);
    for (const x of ["id", "name", "alice", "NULL", "2 rows · 4 ms"]) expect(h).toContain(x);
    expect(h).toMatch(/class="[^"]*null[^"]*"[^>]*>NULL/);
  });
  it("shows only the summary for a statement that changed rows", () => {
    const h = renderToStaticMarkup(<SqlResultView result={{ affected: 3, millis: 2 }} />);
    expect(h).toContain("3 rows affected · 2 ms");
    expect(h).not.toMatch(/<table/);
  });
  it("says so when the rows were cut", () => {
    expect(renderToStaticMarkup(<SqlResultView result={{ ...res, truncated: true }} />)).toContain("(cut)");
  });
});
