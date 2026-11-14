import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Branch, Checkpoint, HistoryTx, Line } from "../api";
import { layoutGraph } from "../lib/historygraph";
import { type Panel, PanelBody, PanelView } from "./checkpoints";

const at = (min: number) => new Date(Date.UTC(2026, 10, 12, 10, min)).toISOString();
const cp = (name: string, min: number, line: number, extra: Partial<Checkpoint> = {}): Checkpoint => ({ name, created_at: at(min), size: 1, line, auto: false, current: false, ...extra });
const tx = (n: number, min: number): HistoryTx => ({ gtid: `u:${n}`, time: at(min), sql: `insert ${n}`, truncated: false, tables: [], inserted: 1, updated: 0, deleted: 0, ddl: false, file: "f" });
const lines: Line[] = [
  { line: 0, live: false, parent: null, from: "" },
  { line: 1, live: true, parent: 0, from: "c2" },
];
const items = [cp("c1", 0, 0), cp("c2", 10, 0), cp("before-restore-1", 20, 0, { auto: true }), cp("d1", 30, 1, { current: true })];
const noop = () => {};

function panel(over: Partial<Panel> = {}): Panel {
  return {
    branch: { name: "x", record: false } as Branch,
    graph: { items, lines, live: 1, head: "d1" },
    rows: [...items].reverse(),
    events: undefined,
    fromTx: undefined,
    error: null,
    note: "",
    warning: "",
    working: false,
    name: "",
    setName: noop,
    problem: null,
    saveWhy: undefined,
    restoreWhy: undefined,
    deleteWhy: undefined,
    txRestoreWhy: undefined,
    save: noop,
    saveFirst: noop,
    selected: null,
    select: noop,
    asking: null,
    setAsking: noop,
    askingTx: false,
    setAskingTx: noop,
    restoreCheckpoint: noop,
    deleteCheckpoint: noop,
    restoreTx: noop,
    shown: (line) => layoutGraph({ lines, checkpoints: items, head: null }).nodes.find((n) => n.line === line)?.shown ?? line,
    showLines: false,
    ...over,
  };
}
const body = (p: Panel, modal = false) => renderToStaticMarkup(<PanelBody p={p} modal={modal} onExpand={noop} />);
const selectedNode = (id: string) => layoutGraph({ lines, checkpoints: items, head: "d1" }).nodes.find((n) => n.id === id)!;

describe("PanelBody inline", () => {
  it("has an Expand button on the graph, with a label a screen reader can use", () => {
    const h = body(panel());
    expect(h).toMatch(/<button[^>]*aria-haspopup="dialog"[^>]*>.*Expand<\/button>/);
    expect(h).toContain("<h3");
    expect(h).toContain("hg");
  });
  it("hides the button when there is nothing to enlarge", () => {
    const h = body(panel({ graph: { items: [], lines: [lines[0]], live: 0, head: null }, rows: [] }));
    expect(h).not.toContain("Expand");
    expect(h).toContain("No checkpoints yet.");
  });
  it("draws the graph for a branch without checkpoints once it has recorded transactions", () => {
    const h = body(panel({ graph: { items: [], lines: [lines[0]], live: 0, head: null }, rows: [], events: { 0: [tx(7, 5)] } }));
    expect(h).toContain("hg");
    expect(h).toContain("Expand");
    expect(h).toContain("Dots are recorded transactions");
    expect(h).toContain("No checkpoints yet.");
  });
  it("draws no graph for recorded branch whose log is still empty", () => {
    const h = body(panel({ graph: { items: [], lines: [lines[0]], live: 0, head: null }, rows: [], events: { 0: [] } }));
    expect(h).not.toContain("Expand");
  });
});

describe("PanelBody in the modal", () => {
  it("shows everything the page shows, enlarged, without a second Expand button or title", () => {
    const h = body(panel({ selected: selectedNode("checkpoint:d1"), error: "boom", note: "Restore started", warning: "low space" }), true);
    expect(h).not.toContain("Expand");
    expect(h).not.toContain("<h3");
    expect(h).toContain("hg big");
    expect(h).toContain("Save checkpoint");
    expect(h).toContain('aria-label="Name (optional)"');
    expect(h).toContain('aria-label="Selected"');
    expect(h).toContain("boom");
    expect(h).toContain("Restore started");
    expect(h).toContain("low space");
    expect(h.match(/>Restore</g)!.length).toBeGreaterThanOrEqual(5); // detail + four rows
    expect(h.match(/>Delete</g)).toHaveLength(4);
  });
  it("shows the confirmation of a restore inside it", () => {
    const h = body(panel({ selected: selectedNode("checkpoint:c1"), asking: { what: "restore", name: "c1", from: "graph" } }), true);
    expect(h).toContain("Confirm restore");
    expect(h).toContain('aria-label="Restore c1"');
  });
  it("says plainly that a restore replaces the state and saves nothing, and offers to save a checkpoint first", () => {
    for (const asking of [{ what: "restore" as const, name: "c1", from: "graph" as const }, { what: "restore" as const, name: "c1", from: "list" as const }]) {
      const h = body(panel({ selected: selectedNode("checkpoint:c1"), asking }), true);
      expect(h).toContain("is replaced and is not saved");
      expect(h).toContain("Save a checkpoint first");
      expect(h).not.toContain("Nothing is lost");
      expect(h).not.toContain("before-restore checkpoint (the newest");
    }
  });
  it("offers the same for a restore to a recorded transaction", () => {
    const g = layoutGraph({ lines, checkpoints: items, head: null, events: { 0: [tx(7, 5)] } });
    const h = body(panel({ events: { 0: [tx(7, 5)] }, selected: g.nodes.find((n) => n.kind === "tx")!, askingTx: true }), true);
    expect(h).toContain("is not saved");
    expect(h).toContain("Save a checkpoint first");
  });
  it("shows a group of transactions as a table in time order and restores the chosen row", () => {
    const burst = [1, 2, 3].map((n) => ({ ...tx(n, 5), time: new Date(Date.UTC(2026, 10, 12, 10, 5, 0, n * 40)).toISOString(), sql: `insert ${n}` }));
    const g = layoutGraph({ lines, checkpoints: items, head: null, events: { 0: burst } });
    const grp = g.nodes.find((n) => n.kind === "group")!;
    expect(grp.group).toHaveLength(3);
    const h = body(panel({ events: { 0: burst }, selected: grp }), true);
    expect(h).toContain("3 transactions");
    expect(h).toMatch(/<table/);
    for (const n of [1, 2, 3]) expect(h).toContain(`insert ${n}`);
    expect(h.indexOf("insert 1")).toBeLessThan(h.indexOf("insert 3"));
    // the last row is chosen first, and has the restore button
    expect(h).toContain("Restore to here");
    expect(h).toMatch(/aria-pressed="true"[^>]*>[^<]*#3/);
  });
  it("does not offer to save when saving is not allowed, and says why", () => {
    const h = body(panel({ selected: selectedNode("checkpoint:c1"), asking: { what: "restore", name: "c1", from: "graph" }, saveWhy: "No permission" }), true);
    expect(h).toMatch(/<button[^>]*disabled[^>]*title="No permission"[^>]*>Save a checkpoint first/);
  });
  it("shows a recorded transaction with its statements and the restore to it", () => {
    const g = layoutGraph({ lines, checkpoints: items, head: null, events: { 0: [tx(7, 5)] } });
    const h = body(panel({ events: { 0: [tx(7, 5)] }, selected: g.nodes.find((n) => n.kind === "tx")! }), true);
    expect(h).toContain("insert 7");
    expect(h).toContain("Restore to here");
    expect(h).toContain("Dots are recorded transactions");
  });
});

describe("lines named the same everywhere", () => {
  it("says the lane a checkpoint is drawn on in the detail and the list, not the line the server numbered", () => {
    const p = panel({ selected: selectedNode("checkpoint:d1"), showLines: true });
    const h = body(p);
    expect(h).not.toContain("line 1");
    expect(h).toContain('muted small">line 0');
  });
  it("leaves the line out of the list while there is only one lane", () => {
    expect(body(panel({ showLines: false }), true)).not.toContain('muted small">line');
  });
});

describe("PanelView", () => {
  const view = (open: boolean) => renderToStaticMarkup(<PanelView p={panel()} open={open} onOpen={noop} onClose={noop} />);
  it("shows the panel on the page, and nothing else while the modal is closed", () => {
    const h = view(false);
    expect(h).not.toContain('role="dialog"');
    expect(h).not.toContain("inert");
  });
  it("opens the modal with the same panel in it, and takes the one on the page out of reach", () => {
    const h = view(true);
    expect(h).toContain('role="dialog"');
    expect(h).toContain("Checkpoints of x");
    expect(h).toMatch(/<section class="cps"[^>]*inert/);
    expect(h.match(/aria-label="Name \(optional\)"/g)).toHaveLength(2);
  });
});
