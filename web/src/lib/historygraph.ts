// The history of a branch drawn as a graph: each line of history is a lane,
// checkpoints are nodes on it, a restore is a branch off the checkpoint it
// restored, and (with the history module) recorded transactions are small dots
// between the checkpoints. Pure layout, so it is tested without a browser.
import type { Checkpoint, HistoryTx, Line } from "../api";

export const SLOT_W = 104;
export const LANE_H = 52;
export const LEFT = 76;
export const TOP = 30;
// Most transaction dots drawn between two neighbours; a busier stretch is sampled.
export const MAX_DOTS = 36;

export type GNode = {
  id: string;
  kind: "reset" | "checkpoint" | "tx";
  x: number;
  y: number;
  lane: number;
  line: number;
  time: number;
  checkpoint?: Checkpoint;
  tx?: HistoryTx;
  head?: boolean;
};
export type GLane = { line: number; lane: number; y: number; x1: number; x2: number; live: boolean };
export type GFork = { line: number; x1: number; y1: number; x2: number; y2: number };
export type GLayout = {
  width: number;
  height: number;
  lanes: GLane[];
  forks: GFork[];
  nodes: GNode[];
  // The end of the live line, where the branch's data is now.
  now: { x: number; y: number; lane: number } | null;
  // sampled: transactions left out of the drawing because a stretch was too busy.
  sampled: number;
};

const ms = (iso: string) => Date.parse(iso);

// sample keeps at most max of the items, spread evenly, in order.
export function sample<T>(items: T[], max: number): T[] {
  if (items.length <= max) return items;
  return Array.from({ length: max }, (_, i) => items[Math.round((i * (items.length - 1)) / (max - 1))]);
}

export function nodeId(kind: GNode["kind"], key: string): string {
  return `${kind}:${key}`;
}

export function layoutGraph(input: {
  lines: Line[];
  checkpoints: Checkpoint[];
  head: string | null;
  events?: Record<number, HistoryTx[]>;
}): GLayout {
  const lines = [...input.lines].sort((a, b) => a.line - b.line);
  const laneOf = new Map(lines.map((l, i) => [l.line, i]));
  const cps = [...input.checkpoints].sort((a, b) => ms(a.created_at) - ms(b.created_at) || (a.name < b.name ? -1 : 1));
  // Chronological slots: the reset point is slot 0, then every checkpoint in time order.
  const slot = new Map<string, number>(cps.map((c, i) => [c.name, i + 1]));
  const xOf = (s: number) => LEFT + s * SLOT_W;
  const yOf = (lane: number) => TOP + lane * LANE_H;
  const lastSlot = cps.length + 1; // where "now" is

  const firstOnLine = new Map<number, Checkpoint>();
  const lastOnLine = new Map<number, Checkpoint>();
  for (const c of cps) {
    if (!firstOnLine.has(c.line)) firstOnLine.set(c.line, c);
    lastOnLine.set(c.line, c);
  }

  const nodes: GNode[] = [];
  const lanes: GLane[] = [];
  const forks: GFork[] = [];
  let now: GLayout["now"] = null;
  let sampled = 0;

  nodes.push({ id: nodeId("reset", "0"), kind: "reset", x: xOf(0), y: yOf(laneOf.get(0) ?? 0), lane: laneOf.get(0) ?? 0, line: 0, time: 0 });

  for (const l of lines) {
    const lane = laneOf.get(l.line)!;
    const y = yOf(lane);
    const first = firstOnLine.get(l.line);
    const last = lastOnLine.get(l.line);
    // Where the lane starts: line 0 at the reset point; others just before
    // their first checkpoint, or at the right end while they have none.
    const startX = l.line === 0 ? xOf(0) : first ? xOf(slot.get(first.name)!) - SLOT_W * 0.5 : xOf(lastSlot) - SLOT_W * 0.5;
    const endX = l.live ? xOf(lastSlot) : last ? xOf(slot.get(last.name)!) : startX;
    lanes.push({ line: l.line, lane, y, x1: startX, x2: Math.max(endX, startX), live: l.live });
    if (l.live) now = { x: Math.max(endX, startX), y, lane };

    if (l.line !== 0 && l.from !== "") {
      const parentLane = laneOf.get(l.parent ?? 0) ?? 0;
      const fromSlot = l.from === "reset" ? 0 : slot.get(l.from);
      if (fromSlot !== undefined) forks.push({ line: l.line, x1: xOf(fromSlot), y1: yOf(parentLane), x2: startX, y2: y });
    }

    for (const c of cps.filter((c) => c.line === l.line)) {
      nodes.push({ id: nodeId("checkpoint", c.name), kind: "checkpoint", x: xOf(slot.get(c.name)!), y, lane, line: l.line, time: ms(c.created_at), checkpoint: c, head: c.name === input.head });
    }

    // Transactions between the lane's neighbours, by time.
    const txs = [...(input.events?.[l.line] ?? [])].sort((a, b) => ms(a.time) - ms(b.time));
    if (txs.length > 0) {
      const anchors: { x: number; t: number }[] = cps.filter((c) => c.line === l.line).map((c) => ({ x: xOf(slot.get(c.name)!), t: ms(c.created_at) }));
      const t0 = ms(txs[0].time) - 1000;
      const tEnd = Math.max(ms(txs[txs.length - 1].time), anchors.length ? anchors[anchors.length - 1].t : 0) + 1000;
      anchors.unshift({ x: startX + 14, t: Math.min(t0, anchors[0]?.t ?? t0) });
      if (l.live || anchors[anchors.length - 1].t < tEnd - 1000) anchors.push({ x: Math.max(endX, anchors[anchors.length - 1].x + SLOT_W * 0.6) - 14, t: tEnd });
      const perInterval = new Map<number, HistoryTx[]>();
      for (const tx of txs) {
        const t = ms(tx.time);
        let i = 0;
        while (i < anchors.length - 2 && t >= anchors[i + 1].t) i++;
        perInterval.set(i, [...(perInterval.get(i) ?? []), tx]);
      }
      for (const [i, list] of perInterval) {
        const a = anchors[i];
        const b = anchors[i + 1] ?? { x: a.x + SLOT_W * 0.6, t: a.t + 1 };
        const kept = sample(list, MAX_DOTS);
        sampled += list.length - kept.length;
        for (const tx of kept) {
          const f = b.t > a.t ? Math.min(1, Math.max(0, (ms(tx.time) - a.t) / (b.t - a.t))) : 0.5;
          const x = a.x + 12 + (b.x - a.x - 24) * f;
          nodes.push({ id: nodeId("tx", `${l.line}:${tx.gtid}`), kind: "tx", x, y, lane, line: l.line, time: ms(tx.time), tx });
        }
      }
    }
  }

  nodes.sort((a, b) => a.lane - b.lane || a.x - b.x);
  return { width: xOf(lastSlot) + SLOT_W * 0.9, height: yOf(Math.max(lines.length - 1, 0)) + 46, lanes, forks, nodes, now, sampled };
}

// Moving around the graph with the arrow keys: the node next to this one on
// its lane, or the closest one on the lane above or below.
export function neighbour(nodes: GNode[], from: GNode, key: "ArrowLeft" | "ArrowRight" | "ArrowUp" | "ArrowDown"): GNode | undefined {
  if (key === "ArrowLeft" || key === "ArrowRight") {
    const row = nodes.filter((n) => n.lane === from.lane).sort((a, b) => a.x - b.x);
    const i = row.findIndex((n) => n.id === from.id);
    return row[i + (key === "ArrowRight" ? 1 : -1)];
  }
  const lane = from.lane + (key === "ArrowDown" ? 1 : -1);
  const row = nodes.filter((n) => n.lane === lane);
  return row.sort((a, b) => Math.abs(a.x - from.x) - Math.abs(b.x - from.x))[0];
}

// describe is the text a screen reader gets for a node.
export function describe(n: GNode): string {
  if (n.kind === "reset") return "Creation state";
  if (n.kind === "checkpoint") return `Checkpoint ${n.checkpoint!.name}, line ${n.line}`;
  return `Transaction ${n.tx!.gtid.split(":").pop()}, line ${n.line}`;
}
