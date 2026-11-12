import { describe, expect, it } from "vitest";
import type { Checkpoint, HistoryTx, Line } from "../api";
import { describe as say, layoutGraph, LANE_H, LEFT, MAX_DOTS, neighbour, sample, SLOT_W } from "./historygraph";

const at = (min: number) => new Date(Date.UTC(2026, 10, 12, 10, min)).toISOString();
const cp = (name: string, min: number, line: number, extra: Partial<Checkpoint> = {}): Checkpoint => ({ name, created_at: at(min), size: 1, line, auto: false, current: false, ...extra });
const tx = (n: number, min: number): HistoryTx => ({ gtid: `u:${n}`, time: at(min), sql: `insert ${n}`, truncated: false, tables: [], inserted: 1, updated: 0, deleted: 0, ddl: false, file: "binlog.000001" });

// c1 (line 0) and c2 (line 0); the branch was restored to c1: line 1 (live) starts there.
const lines: Line[] = [
  { line: 0, live: false, parent: null, from: "" },
  { line: 1, live: true, parent: 0, from: "c1" },
];
const cps = [cp("c1", 0, 0), cp("c2", 10, 0), cp("before-restore-x", 20, 0, { auto: true }), cp("d1", 30, 1, { current: true })];

describe("layoutGraph", () => {
  it("puts each line on its own lane and the checkpoints in time order from left to right", () => {
    const g = layoutGraph({ lines, checkpoints: cps, head: "d1" });
    expect(g.lanes.map((l) => [l.line, l.y])).toEqual([[0, 30], [1, 30 + LANE_H]]);
    const xs = g.nodes.filter((n) => n.kind === "checkpoint").map((n) => [n.checkpoint!.name, n.x]);
    expect(xs).toEqual([["c1", LEFT + SLOT_W], ["c2", LEFT + 2 * SLOT_W], ["before-restore-x", LEFT + 3 * SLOT_W], ["d1", LEFT + 4 * SLOT_W]]);
    expect(g.nodes.find((n) => n.kind === "reset")).toMatchObject({ x: LEFT, lane: 0 });
    expect(g.nodes.find((n) => n.head)?.checkpoint?.name).toBe("d1");
  });

  it("draws a restore as a branch off the checkpoint it came from", () => {
    const g = layoutGraph({ lines, checkpoints: cps, head: "d1" });
    expect(g.forks).toHaveLength(1);
    const f = g.forks[0];
    expect(f).toMatchObject({ line: 1, x1: LEFT + SLOT_W, y1: 30, y2: 30 + LANE_H });
    // The new lane starts just before its first checkpoint, and the live lane ends at "now".
    expect(f.x2).toBe(LEFT + 4 * SLOT_W - SLOT_W / 2);
    expect(g.now).toEqual({ x: LEFT + 5 * SLOT_W, y: 30 + LANE_H, lane: 1 });
    expect(g.lanes[0].x2).toBe(LEFT + 3 * SLOT_W); // the set-aside line ends at its last checkpoint
  });

  it("starts a live line that has no checkpoint yet at the right end, and a restore of the creation state at the reset point", () => {
    const l: Line[] = [{ line: 0, live: false, parent: null, from: "" }, { line: 1, live: true, parent: 0, from: "reset" }];
    const g = layoutGraph({ lines: l, checkpoints: [cp("a", 0, 0)], head: null });
    expect(g.forks[0]).toMatchObject({ x1: LEFT, y1: 30, x2: LEFT + 2 * SLOT_W - SLOT_W / 2 });
    expect(g.lanes[1].x1).toBe(LEFT + 2 * SLOT_W - SLOT_W / 2);
    expect(g.now?.lane).toBe(1);
  });

  it("copes with a branch that has no checkpoints, a missing origin and lines out of order", () => {
    const empty = layoutGraph({ lines: [{ line: 0, live: true, parent: null, from: "" }], checkpoints: [], head: null });
    expect(empty.nodes.map((n) => n.kind)).toEqual(["reset"]);
    expect(empty.now).toEqual({ x: LEFT + SLOT_W, y: 30, lane: 0 });
    const none = layoutGraph({ lines: [], checkpoints: [], head: null });
    expect(none.lanes).toEqual([]);
    expect(none.nodes[0]).toMatchObject({ kind: "reset", lane: 0 });
    // The checkpoint a line came from is gone: no fork is drawn, the lane still is.
    const gone = layoutGraph({ lines: [{ line: 0, live: false, parent: null, from: "" }, { line: 1, live: true, parent: 0, from: "deleted" }], checkpoints: [cp("a", 0, 0)], head: null });
    expect(gone.forks).toEqual([]);
    const shuffled = layoutGraph({ lines: [lines[1], lines[0]], checkpoints: cps, head: null });
    expect(shuffled.lanes.map((l) => l.line)).toEqual([0, 1]);
  });

  it("places transactions between the checkpoints they happened between", () => {
    const events = { 0: [tx(1, 5), tx(2, 15), tx(3, 25)], 1: [tx(9, 40)] };
    const g = layoutGraph({ lines, checkpoints: cps, head: "d1", events });
    const dot = (line: number, n: number) => g.nodes.find((x) => x.kind === "tx" && x.id === `tx:${line}:u:${n}`)!;
    // 5 minutes after c1 (0) and 5 before c2 (10): between them.
    expect(dot(0, 1).x).toBeGreaterThan(LEFT + SLOT_W);
    expect(dot(0, 1).x).toBeLessThan(LEFT + 2 * SLOT_W);
    expect(dot(0, 2).x).toBeGreaterThan(LEFT + 2 * SLOT_W);
    expect(dot(0, 2).x).toBeLessThan(LEFT + 3 * SLOT_W);
    expect(dot(0, 3).x).toBeGreaterThan(LEFT + 3 * SLOT_W - 1); // after the last checkpoint: to its right (not past the lane)
    expect(dot(1, 9).x).toBeGreaterThan(LEFT + 4 * SLOT_W);
    expect(dot(1, 9).y).toBe(30 + LANE_H);
    expect(g.sampled).toBe(0);
  });

  it("puts transactions before the first checkpoint of a line, and on a line without checkpoints", () => {
    const g = layoutGraph({ lines, checkpoints: cps, head: null, events: { 0: [tx(1, -30)], 1: [tx(2, 29)] } });
    const first = g.nodes.find((n) => n.id === "tx:0:u:1")!;
    expect(first.x).toBeLessThan(LEFT + SLOT_W);
    expect(first.x).toBeGreaterThanOrEqual(LEFT);
    const bare = layoutGraph({ lines: [{ line: 0, live: true, parent: null, from: "" }], checkpoints: [], head: null, events: { 0: [tx(1, 1), tx(2, 2)] } });
    expect(bare.nodes.filter((n) => n.kind === "tx")).toHaveLength(2);
    const [a, b] = bare.nodes.filter((n) => n.kind === "tx");
    expect(a.x).toBeLessThan(b.x);
  });

  it("samples a stretch with too many transactions and says how many were left out", () => {
    const many = Array.from({ length: MAX_DOTS + 14 }, (_, i) => tx(i + 1, 1 + (i % 8)));
    const g = layoutGraph({ lines, checkpoints: cps, head: null, events: { 0: many } });
    expect(g.nodes.filter((n) => n.kind === "tx")).toHaveLength(MAX_DOTS);
    expect(g.sampled).toBe(14);
    expect(sample([1, 2, 3], 5)).toEqual([1, 2, 3]);
    expect(sample([1, 2, 3, 4, 5, 6, 7, 8, 9], 3)).toEqual([1, 5, 9]);
  });

  it("orders the nodes by lane and position, and sizes the picture to them", () => {
    const g = layoutGraph({ lines, checkpoints: cps, head: null, events: { 0: [tx(1, 5)] } });
    const order = g.nodes.map((n) => [n.lane, n.x]);
    expect(order).toEqual([...order].sort((a, b) => a[0] - b[0] || a[1] - b[1]));
    expect(g.width).toBeGreaterThan(Math.max(...g.nodes.map((n) => n.x)));
    expect(g.height).toBeGreaterThan(30 + LANE_H);
  });
});

describe("neighbour", () => {
  const g = layoutGraph({ lines, checkpoints: cps, head: null, events: { 0: [tx(1, 5)] } });
  const byId = (id: string) => g.nodes.find((n) => n.id === id)!;
  it("moves along a lane with left and right and stops at its ends", () => {
    expect(neighbour(g.nodes, byId("checkpoint:c1"), "ArrowRight")?.id).toBe("tx:0:u:1");
    expect(neighbour(g.nodes, byId("tx:0:u:1"), "ArrowRight")?.id).toBe("checkpoint:c2");
    expect(neighbour(g.nodes, byId("tx:0:u:1"), "ArrowLeft")?.id).toBe("checkpoint:c1");
    expect(neighbour(g.nodes, byId("reset:0"), "ArrowLeft")).toBeUndefined();
  });
  it("moves to the closest node of the lane above or below", () => {
    expect(neighbour(g.nodes, byId("checkpoint:c2"), "ArrowDown")?.lane).toBe(1);
    expect(neighbour(g.nodes, byId("checkpoint:d1"), "ArrowUp")?.id).toBe("checkpoint:before-restore-x");
    expect(neighbour(g.nodes, byId("checkpoint:c1"), "ArrowUp")).toBeUndefined();
  });
});

describe("describe", () => {
  it("names a node for a screen reader", () => {
    const g = layoutGraph({ lines, checkpoints: cps, head: null, events: { 0: [tx(7, 5)] } });
    expect(g.nodes.map(say)).toContain("Creation state");
    expect(g.nodes.map(say)).toContain("Checkpoint c1, line 0");
    expect(g.nodes.map(say)).toContain("Transaction 7, line 0");
  });
});
