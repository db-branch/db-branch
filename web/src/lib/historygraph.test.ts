import { describe, expect, it } from "vitest";
import type { Checkpoint, HistoryTx, Line } from "../api";
import { describe as say, FORK_START, forkPath, GROUP_W, layoutGraph, LANE_H, LEFT, MIN_DOT_GAP, mergeLines, neighbour, SLOT_W } from "./historygraph";

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
  it("puts each line on its own lane and the checkpoints of a lane in time order from left to right", () => {
    const g = layoutGraph({ lines, checkpoints: cps, head: "d1" });
    expect(g.lanes.map((l) => [l.line, l.y])).toEqual([[0, 30], [1, 30 + LANE_H]]);
    const xs = g.nodes.filter((n) => n.kind === "checkpoint").map((n) => [n.checkpoint!.name, n.x]);
    expect(xs).toEqual([["c1", LEFT + SLOT_W], ["c2", LEFT + 2 * SLOT_W], ["before-restore-x", LEFT + 3 * SLOT_W], ["d1", LEFT + SLOT_W + FORK_START + SLOT_W / 2]]);
    expect(g.nodes.find((n) => n.kind === "reset")).toMatchObject({ x: LEFT, lane: 0 });
    expect(g.nodes.find((n) => n.head)?.checkpoint?.name).toBe("d1");
  });

  it("draws a restore as a branch off the checkpoint it came from", () => {
    const g = layoutGraph({ lines, checkpoints: cps, head: "d1" });
    expect(g.forks).toHaveLength(1);
    const f = g.forks[0];
    expect(f).toMatchObject({ line: 1, x1: LEFT + SLOT_W, y1: 30, y2: 30 + LANE_H });
    // The new lane starts a short way right of where it forked, and the live lane ends at "now".
    expect(f.x2).toBe(LEFT + SLOT_W + FORK_START);
    expect(g.now).toEqual({ x: LEFT + SLOT_W + FORK_START + SLOT_W / 2 + SLOT_W, y: 30 + LANE_H, lane: 1 });
    expect(g.lanes[0].x2).toBe(LEFT + 3 * SLOT_W); // the set-aside line ends at its last checkpoint
  });

  it("starts a live line that has no checkpoint yet next to where it forked, and a restore of the creation state next to the reset point", () => {
    const l: Line[] = [{ line: 0, live: false, parent: null, from: "" }, { line: 1, live: true, parent: 0, from: "reset" }];
    const g = layoutGraph({ lines: l, checkpoints: [cp("a", 0, 0)], head: null });
    expect(g.forks[0]).toMatchObject({ x1: LEFT, y1: 30, x2: LEFT + FORK_START });
    expect(g.lanes[1].x1).toBe(LEFT + FORK_START);
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
    expect(dot(1, 9).x).toBeGreaterThan(LEFT + SLOT_W + FORK_START + SLOT_W / 2);
    expect(dot(1, 9).y).toBe(30 + LANE_H);
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

  const tsAt = (n: number, sec: number, extra: Partial<HistoryTx> = {}): HistoryTx => ({ ...tx(n, 5), time: new Date(Date.UTC(2026, 10, 12, 10, 5, 0, sec * 1000)).toISOString(), ...extra });
  it("draws transactions made within a second of each other as one group, in time order", () => {
    const g = layoutGraph({ lines, checkpoints: cps, head: null, events: { 0: [tsAt(1, 0), tsAt(2, 0.04), tsAt(3, 0.08), tsAt(4, 30)] } });
    const dots = g.nodes.filter((n) => n.kind === "group" || n.kind === "tx").filter((n) => n.line === 0);
    expect(dots.map((n) => n.kind).sort()).toEqual(["group", "tx"]);
    const grp = dots.find((n) => n.kind === "group")!;
    expect(grp.group!.map((x) => x.gtid)).toEqual(["u:1", "u:2", "u:3"]);
    expect(grp.tx!.gtid).toBe("u:3"); // the last one: the state after the group
    expect(say(grp)).toBe("3 transactions, #1 to #3, line 0");
  });
  it("chains a group while each step is within a second, and splits a longer pause", () => {
    const g = layoutGraph({ lines, checkpoints: cps, head: null, events: { 0: [tsAt(1, 0), tsAt(2, 0.9), tsAt(3, 1.8), tsAt(4, 3.5), tsAt(5, 3.6)] } });
    expect(g.nodes.filter((n) => n.kind === "group").map((n) => n.group!.length).sort()).toEqual([2, 3]);
  });
  it("keeps a DDL statement on its own and splits a group around it", () => {
    const g = layoutGraph({ lines, checkpoints: cps, head: null, events: { 0: [tsAt(1, 0), tsAt(2, 0.1, { ddl: true, inserted: 0 }), tsAt(3, 0.2), tsAt(4, 0.3)] } });
    expect(g.nodes.filter((n) => n.kind === "tx").map((n) => n.tx!.gtid).sort()).toEqual(["u:1", "u:2"]);
    expect(g.nodes.filter((n) => n.kind === "group").map((n) => n.group!.map((x) => x.gtid))).toEqual([["u:3", "u:4"]]);
  });
  it("does not group across a checkpoint of the line", () => {
    const at5 = [cp("k1", 5, 0)];
    const g = layoutGraph({ lines: [lines[0]], checkpoints: at5, head: null, events: { 0: [tsAt(1, -0.2), tsAt(2, 0.2)] } });
    expect(g.nodes.filter((n) => n.kind === "group")).toHaveLength(0);
    expect(g.nodes.filter((n) => n.kind === "tx")).toHaveLength(2);
  });
  it("keeps a minimum gap between dots and groups", () => {
    const g = layoutGraph({ lines, checkpoints: cps, head: null, events: { 0: [tsAt(1, 0), tsAt(2, 1.5), tsAt(3, 3), tsAt(4, 4.5)] } });
    const xs = g.nodes.filter((n) => n.kind === "tx" && n.line === 0).map((n) => n.x).sort((a, b) => a - b);
    for (let i = 1; i < xs.length; i++) expect(xs[i] - xs[i - 1]).toBeGreaterThanOrEqual(12);
  });
  it("merges close neighbours into groups when a stretch is too short for all its dots, and loses none", () => {
    const many = Array.from({ length: 40 }, (_, i) => tsAt(i + 1, 3 * i));
    const g = layoutGraph({ lines, checkpoints: cps, head: null, events: { 0: many } });
    const dots = g.nodes.filter((n) => (n.kind === "tx" || n.kind === "group") && n.line === 0).sort((a, b) => a.x - b.x);
    // every transaction is in exactly one node
    const all = dots.flatMap((n) => (n.group ?? [n.tx!]).map((x) => x.gtid)).sort();
    expect(all).toEqual(many.map((x) => x.gtid).sort());
    expect(dots.some((n) => n.kind === "group")).toBe(true);
    // nodes do not lie on top of each other: a dot needs 14px, a group (with its count) 26px
    const w = (n: (typeof dots)[number]) => (n.kind === "group" ? GROUP_W : MIN_DOT_GAP);
    for (let i = 1; i < dots.length; i++) expect(dots[i].x - dots[i - 1].x).toBeGreaterThanOrEqual((w(dots[i]) + w(dots[i - 1])) / 2 - 0.01);
  });
  it("merges the pair with the least time between first and never merges a DDL statement", () => {
    const rows = [tsAt(1, 0), tsAt(2, 100), tsAt(3, 100.5, { ddl: true, inserted: 0 }), tsAt(4, 101.5), tsAt(5, 300)];
    const g = layoutGraph({ lines: [lines[0]], checkpoints: [], head: null, events: { 0: rows } });
    const nodes = g.nodes.filter((n) => n.kind === "tx" || n.kind === "group");
    expect(nodes.flatMap((n) => (n.group ?? [n.tx!]).map((x) => x.gtid)).sort()).toEqual(["u:1", "u:2", "u:3", "u:4", "u:5"]);
    expect(nodes.filter((n) => n.kind === "group").every((n) => n.group!.every((x) => !x.ddl))).toBe(true);
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
    const up = neighbour(g.nodes, byId("checkpoint:d1"), "ArrowUp")!;
    expect(up.lane).toBe(0);
    // it is the closest of the lane above, whatever its kind
    const d1 = byId("checkpoint:d1").x;
    for (const n of g.nodes.filter((n) => n.lane === 0)) expect(Math.abs(up.x - d1)).toBeLessThanOrEqual(Math.abs(n.x - d1));
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

// Restoring the tip of a line extends the line instead of forking a new lane.
describe("layoutGraph: a restore of the tip extends its lane", () => {
  const auto = (name: string, min: number, line: number) => cp(name, min, line, { auto: true });
  const root: Line = { line: 0, live: false, parent: null, from: "" };
  // c1, c2 (the tip) on line 0; c2 restored: the automatic b1 is taken on line 0 and line 1 continues.
  const tipLines: Line[] = [root, { line: 1, live: true, parent: 0, from: "c2" }];
  const tipCps = [cp("c1", 0, 0), cp("c2", 10, 0), auto("before-restore-1", 20, 0), cp("d1", 30, 1, { current: true })];
  const laneOf = (g: ReturnType<typeof layoutGraph>, id: string) => g.nodes.find((n) => n.id === id)!.lane;

  it("keeps the restored state on the same lane: no fork, no new label, the lane runs on to now", () => {
    const g = layoutGraph({ lines: tipLines, checkpoints: tipCps, head: "d1" });
    expect(g.lanes).toHaveLength(1);
    expect(g.forks).toEqual([]);
    expect(g.lanes[0]).toMatchObject({ line: 0, lane: 0, y: 30, live: true, x2: LEFT + 5 * SLOT_W });
    expect(g.nodes.every((n) => n.lane === 0 && n.y === 30)).toBe(true);
    expect(g.now).toEqual({ x: LEFT + 5 * SLOT_W, y: 30, lane: 0 });
    expect(g.height).toBe(30 + 46);
    expect(g.nodes.find((n) => n.head)?.checkpoint?.name).toBe("d1");
  });

  it("names the line a node is on by the lane it is drawn on, and keeps the line it really belongs to for the API", () => {
    const g = layoutGraph({ lines: tipLines, checkpoints: tipCps, head: null });
    const d1 = g.nodes.find((n) => n.id === "checkpoint:d1")!;
    expect(d1).toMatchObject({ line: 1, shown: 0 });
    expect(say(d1)).toBe("Checkpoint d1, line 0");
    expect(mergeLines({ lines: tipLines, checkpoints: tipCps, head: null })).toEqual(new Map([[0, 0], [1, 0]]));
  });

  it("lets the arrow keys walk across the merged lane", () => {
    const g = layoutGraph({ lines: tipLines, checkpoints: tipCps, head: null });
    const byId = (id: string) => g.nodes.find((n) => n.id === id)!;
    expect(neighbour(g.nodes, byId("checkpoint:c2"), "ArrowRight")?.id).toBe("checkpoint:before-restore-1");
    expect(neighbour(g.nodes, byId("checkpoint:before-restore-1"), "ArrowRight")?.id).toBe("checkpoint:d1");
    expect(neighbour(g.nodes, byId("checkpoint:d1"), "ArrowDown")).toBeUndefined();
  });

  it("extends the lane again and again for a chain of tip restores", () => {
    const l: Line[] = [root, { line: 1, live: false, parent: 0, from: "c2" }, { line: 2, live: true, parent: 1, from: "d1" }];
    const c = [cp("c1", 0, 0), cp("c2", 10, 0), auto("before-restore-1", 20, 0), cp("d1", 30, 1), auto("before-restore-2", 40, 1), cp("e1", 50, 2)];
    const g = layoutGraph({ lines: l, checkpoints: c, head: "e1" });
    expect(g.lanes).toHaveLength(1);
    expect(g.forks).toEqual([]);
    expect(g.nodes.filter((n) => n.kind === "checkpoint").map((n) => n.shown)).toEqual([0, 0, 0, 0, 0, 0]);
    expect(mergeLines({ lines: l, checkpoints: c, head: null })).toEqual(new Map([[0, 0], [1, 0], [2, 0]]));
  });

  it("still forks a new lane for a checkpoint in the middle of the line", () => {
    const l: Line[] = [root, { line: 1, live: true, parent: 0, from: "c1" }];
    const g = layoutGraph({ lines: l, checkpoints: [cp("c1", 0, 0), cp("c2", 10, 0), auto("before-restore-1", 20, 0), cp("d1", 30, 1)], head: "d1" });
    expect(g.lanes.map((x) => x.line)).toEqual([0, 1]);
    expect(g.forks).toHaveLength(1);
    expect(g.nodes.find((n) => n.id === "checkpoint:d1")).toMatchObject({ lane: 1, shown: 1 });
  });

  it("counts a later checkpoint made by hand or by an upgrade, but not the automatic one a restore took", () => {
    const l: Line[] = [root, { line: 1, live: true, parent: 0, from: "c1" }];
    const forks = (c: Checkpoint[]) => layoutGraph({ lines: l, checkpoints: [cp("c1", 0, 0), ...c, cp("d1", 30, 1)], head: null }).forks.length;
    expect(forks([auto("before-restore-1", 20, 0)])).toBe(0);
    expect(forks([cp("c2", 10, 0), auto("before-restore-1", 20, 0)])).toBe(1);
    expect(forks([auto("before-upgrade-1", 10, 0), auto("before-restore-1", 20, 0)])).toBe(1);
    expect(forks([cp("before-restore-by-hand", 10, 0), auto("before-restore-1", 20, 0)])).toBe(1);
  });

  it("forks when the restored checkpoint has a recorded transaction after it, and extends when the last transaction came before it", () => {
    const l: Line[] = [root, { line: 1, live: true, parent: 0, from: "c2" }];
    const run = (events: Record<number, HistoryTx[]> | undefined, recorded = true) => layoutGraph({ lines: l, checkpoints: tipCps, head: null, events, recorded });
    expect(run({ 0: [tx(1, 5)], 1: [] }).forks).toHaveLength(0);
    expect(run({ 0: [tx(1, 12)], 1: [] }).forks).toHaveLength(1);
    expect(run({ 0: [], 1: [] }).lanes).toHaveLength(1);
  });

  it("does not guess about a recorded branch whose transactions it cannot see", () => {
    const l: Line[] = [root, { line: 1, live: true, parent: 0, from: "c2" }];
    expect(layoutGraph({ lines: l, checkpoints: tipCps, head: null, recorded: true }).lanes).toHaveLength(2);
    expect(layoutGraph({ lines: l, checkpoints: tipCps, head: null, recorded: true, events: { 1: [] } }).lanes).toHaveLength(2);
    expect(layoutGraph({ lines: l, checkpoints: tipCps, head: null, recorded: false }).lanes).toHaveLength(1);
  });

  it("forks when a checkpoint was saved on the continued lane after the restore, and the new restore is of an earlier one", () => {
    const l: Line[] = [root, { line: 1, live: false, parent: 0, from: "c2" }, { line: 2, live: true, parent: 0, from: "c2" }];
    const c = [cp("c1", 0, 0), cp("c2", 10, 0), auto("before-restore-1", 20, 0), cp("d1", 30, 1), auto("before-restore-2", 40, 1), cp("e1", 50, 2)];
    const g = layoutGraph({ lines: l, checkpoints: c, head: "e1" });
    expect(g.lanes.map((x) => x.line)).toEqual([0, 2]);
    expect(g.forks).toHaveLength(1);
    expect(g.forks[0]).toMatchObject({ line: 2, x1: LEFT + 2 * SLOT_W, y1: 30, y2: 30 + LANE_H });
    expect(laneOf(g, "checkpoint:d1")).toBe(0);
    expect(laneOf(g, "checkpoint:e1")).toBe(1);
  });

  it("forks off the lane a merged line is drawn on, and keeps the real line numbers on the lanes it forks", () => {
    const l: Line[] = [root, { line: 1, live: false, parent: 0, from: "c2" }, { line: 2, live: true, parent: 1, from: "d1" }];
    const c = [cp("c1", 0, 0), cp("c2", 10, 0), auto("before-restore-1", 20, 0), cp("d1", 30, 1), cp("d2", 35, 1), auto("before-restore-2", 40, 1), cp("e1", 50, 2)];
    const g = layoutGraph({ lines: l, checkpoints: c, head: "e1" });
    expect(g.lanes.map((x) => [x.line, x.lane])).toEqual([[0, 0], [2, 1]]);
    expect(g.forks[0]).toMatchObject({ line: 2, y1: 30, y2: 30 + LANE_H });
    expect(g.nodes.find((n) => n.id === "checkpoint:e1")).toMatchObject({ line: 2, shown: 2, lane: 1 });
  });

  it("does not merge a line whose origin is unknown, or a restore of the creation state that is not the end of the line", () => {
    const old: Line[] = [root, { line: 1, live: true, parent: null, from: "c2" }];
    expect(layoutGraph({ lines: old, checkpoints: tipCps, head: null }).lanes).toHaveLength(2);
    const first: Line[] = [root, { line: 1, live: true, parent: 0, from: "reset" }];
    expect(layoutGraph({ lines: first, checkpoints: [cp("a", 0, 0), auto("before-restore-1", 5, 0)], head: null }).lanes).toHaveLength(2);
    expect(layoutGraph({ lines: first, checkpoints: [auto("before-restore-1", 5, 0), cp("d", 9, 1)], head: null }).lanes).toHaveLength(1);
  });

  it("places the transaction dots of a merged line on the shared lane", () => {
    const g = layoutGraph({ lines: tipLines, checkpoints: tipCps, head: null, events: { 0: [tx(1, 5)], 1: [tx(2, 40)] } });
    const dots = g.nodes.filter((n) => n.kind === "tx");
    expect(dots.map((n) => [n.id, n.lane, n.shown])).toEqual([["tx:0:u:1", 0, 0], ["tx:1:u:2", 0, 0]]);
    expect(dots[0].x).toBeLessThan(dots[1].x);
  });

  it("leaves a set-aside lane that ends in a fork where it was", () => {
    // After the tip restore, line 1 was left by a restore of c1: line 2 forks, and lane 0 now stops at the last checkpoint of line 1.
    const l: Line[] = [root, { line: 1, live: false, parent: 0, from: "c2" }, { line: 2, live: true, parent: 0, from: "c1" }];
    const c = [...tipCps.slice(0, 4), auto("before-restore-2", 40, 1), cp("e1", 50, 2)];
    const g = layoutGraph({ lines: l, checkpoints: c, head: "e1" });
    expect(g.lanes[0]).toMatchObject({ line: 0, live: false, x2: LEFT + 5 * SLOT_W });
    expect(g.lanes[1]).toMatchObject({ line: 2, live: true });
  });
});

describe("layoutGraph: a restore to a recorded transaction (from_tx)", () => {
  const root: Line = { line: 0, live: false, parent: null, from: "" };
  const lines2: Line[] = [root, { line: 1, live: true, parent: 0, from: "c1" }];
  const cps2 = [cp("c1", 0, 0), cp("d1", 40, 1, { current: true })];
  // What the module returns by default: line 0 lost its tail after its last checkpoint
  // (c1 at minute 0) and line 1's own part is the replayed 1..3 and its own 9.
  const parentTxs = [tx(1, -30)];
  const run = (fromTx: Record<number, string> | undefined, extra: Checkpoint[] = [], parent = parentTxs, own = [tx(2, 10), tx(3, 20), tx(4, 30), tx(9, 50)]) =>
    layoutGraph({ lines: lines2, checkpoints: [...cps2, ...extra], head: "d1", events: { 0: parent, 1: own }, recorded: true, fromTx });

  it("extends the lane when the restored transaction is the last one the lane has", () => {
    const g = run({ 1: "u:4" });
    expect(g.lanes).toHaveLength(1);
    expect(g.forks).toEqual([]);
    expect(g.nodes.every((n) => n.lane === 0 && n.shown === 0)).toBe(true);
  });

  it("keeps the real line of a dot for the API, and draws the replayed ones on the shared lane", () => {
    const g = run({ 1: "u:4" });
    const dots = g.nodes.filter((n) => n.kind === "tx" || n.kind === "group");
    expect(dots.flatMap((n) => (n.group ?? [n.tx!]).map((x) => x.gtid)).sort()).toEqual(["u:1", "u:2", "u:3", "u:4", "u:9"]);
    expect(dots.find((n) => (n.group ?? [n.tx!]).some((x) => x.gtid === "u:4"))!.line).toBe(1);
    expect(dots.find((n) => (n.group ?? [n.tx!]).some((x) => x.gtid === "u:1"))!.line).toBe(0);
    expect(dots.every((n) => n.shown === 0)).toBe(true);
  });

  it("forks without from_tx (a restore of the checkpoint, or an older module)", () => {
    const behind = [tx(1, -30), tx(5, 25)];
    expect(run(undefined, [], behind).forks).toHaveLength(1);
    expect(run({}, [], behind).forks).toHaveLength(1);
  });

  it("forks when the lane has a transaction after the restored one", () => {
    expect(run({ 1: "u:3" }, [], [tx(1, -30), tx(5, 25)]).forks).toHaveLength(1);
  });

  it("forks when a checkpoint was saved on the lane after the restored transaction", () => {
    expect(run({ 1: "u:4" }, [cp("c2", 35, 0)]).forks).toHaveLength(1);
  });

  it("falls back to the checkpoint when from_tx is not among the line's transactions", () => {
    // Its transaction may be the checkpoint's own last one, which the line does not list.
    const g = run({ 1: "u:99" });
    expect(g.lanes).toHaveLength(1);
  });

  it("does not guess when the transactions of the lane are not all known", () => {
    const g = layoutGraph({ lines: lines2, checkpoints: cps2, head: "d1", events: { 1: [tx(2, 10)] }, recorded: true, fromTx: { 1: "u:2" } });
    expect(g.forks).toHaveLength(1);
  });

  it("extends again for a chain", () => {
    const three: Line[] = [root, { line: 1, live: false, parent: 0, from: "c1" }, { line: 2, live: true, parent: 0, from: "c1" }];
    const g = layoutGraph({
      lines: three,
      checkpoints: [cp("c1", 0, 0), cp("d1", 25, 1), cp("e1", 60, 2, { current: true })],
      head: "e1",
      events: { 0: parentTxs, 1: [tx(2, 10), tx(3, 20)], 2: [tx(4, 30)] },
      recorded: true,
      fromTx: { 1: "u:3", 2: "u:4" },
    });
    expect(g.lanes).toHaveLength(1);
    expect(g.forks).toEqual([]);
  });
});

describe("forkPath", () => {
  it("leaves the parent lane at a steep angle, then runs straight along the new lane", () => {
    // down 52px over 14px across: a bend close to a right angle, then a horizontal run
    expect(forkPath({ line: 1, x1: 180, y1: 30, x2: 440, y2: 82 })).toBe("M 180 30 L 194 82 L 440 82");
  });
  it("goes up to a lane above the same way, and shortens the slant when there is little room", () => {
    expect(forkPath({ line: 1, x1: 180, y1: 82, x2: 440, y2: 30 })).toBe("M 180 82 L 194 30 L 440 30");
    expect(forkPath({ line: 1, x1: 180, y1: 30, x2: 188, y2: 82 })).toBe("M 180 30 L 188 82");
  });
});

describe("layoutGraph: a forked lane starts next to where it forked", () => {
  const forked: Line[] = [{ line: 0, live: false, parent: null, from: "" }, { line: 1, live: true, parent: 0, from: "c1" }];
  // c1, c2, c3 on line 0 (c1 restored); line 1 has d1, d2 made much later.
  const cpsF = [cp("c1", 0, 0), cp("c2", 10, 0), cp("c3", 20, 0), cp("d1", 60, 1), cp("d2", 70, 1, { current: true })];
  it("puts the first checkpoint of the new line a short way right of the fork, not at its place in time", () => {
    const g = layoutGraph({ lines: forked, checkpoints: cpsF, head: "d2" });
    const x = (id: string) => g.nodes.find((n) => n.id === id)!.x;
    const fork = g.forks[0];
    expect(fork.x1).toBe(x("checkpoint:c1"));
    expect(fork.x2).toBe(fork.x1 + FORK_START);
    expect(x("checkpoint:d1")).toBe(fork.x2 + SLOT_W * 0.5);
    expect(x("checkpoint:d2")).toBe(x("checkpoint:d1") + SLOT_W);
    expect(x("checkpoint:d1")).toBeLessThan(x("checkpoint:c3")); // earlier than c3 on the other lane's scale
  });
  it("runs the live lane on to now one slot after its last checkpoint, and sizes the picture to the widest lane", () => {
    const g = layoutGraph({ lines: forked, checkpoints: cpsF, head: "d2" });
    const d2 = g.nodes.find((n) => n.id === "checkpoint:d2")!.x;
    expect(g.now).toEqual({ x: d2 + SLOT_W, y: LANE_H + 30, lane: 1 });
    const c3 = g.nodes.find((n) => n.id === "checkpoint:c3")!.x;
    expect(g.width).toBe(Math.max(d2 + SLOT_W, c3) + SLOT_W * 0.9);
  });
  it("places a fork from the creation state right next to the reset point", () => {
    const g = layoutGraph({ lines: [forked[0], { line: 1, live: true, parent: 0, from: "reset" }], checkpoints: [cp("c1", 0, 0), cp("d1", 60, 1)], head: null });
    expect(g.forks[0].x1).toBe(LEFT);
    expect(g.forks[0].x2).toBe(LEFT + FORK_START);
  });
});
