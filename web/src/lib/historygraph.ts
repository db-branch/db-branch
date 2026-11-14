// The history of a branch drawn as a graph: each line of history is a lane,
// checkpoints are nodes on it, a restore is a branch off the checkpoint it
// restored, and (with the history module) recorded transactions are small dots
// between the checkpoints. Pure layout, so it is tested without a browser.
//
// A restore normally starts a new line of history, drawn as a new lane that
// forks off the restored checkpoint. When the restored point is the tip of its
// lane, nothing is left behind, so the restore does not fork: its line is
// drawn on the lane it came from (see mergeLines).
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
  // line is the API's number of the line the node belongs to (what a restore
  // from a transaction needs); shown is the line of the lane it is drawn on,
  // which differs when its line continues the lane of an earlier one.
  line: number;
  shown: number;
  time: number;
  checkpoint?: Checkpoint;
  tx?: HistoryTx;
  head?: boolean;
};
// line of a lane is the first line drawn on it; live is true when the branch runs on any line of it.
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

// The checkpoints a restore takes by itself before it swaps the data: a safety
// copy of what was left, not progress of the line.
const isRestoreCopy = (c: Checkpoint) => c.auto && c.name.startsWith("before-restore-");

type LayoutInput = {
  lines: Line[];
  checkpoints: Checkpoint[];
  head: string | null;
  events?: Record<number, HistoryTx[]>;
  // recorded: the branch records its transactions (history module). When it does
  // and they are not known, the end of a line cannot be judged.
  recorded?: boolean;
};

// mergeLines says, for every line, the line whose lane it is drawn on: itself,
// or the line it continues when its restore did not leave anything behind.
//
// The rule. A line N, made by restoring the point `from` on line `parent`, is
// drawn on the lane of `parent` (no fork, no label, same colour) exactly when
//  1. its origin is known: parent is not null, and from is "reset" or a
//     checkpoint of line parent that still exists;
//  2. `from` is the tip of that lane at the time of the restore: no node of
//     the lane is later than it. The lane is the lines already drawn on it
//     (lines numbered below N; later lines did not exist yet). Later nodes are
//     the checkpoints (in the order of the graph: time, then name) and the
//     recorded transactions (time) of those lines, except the automatic
//     before-restore-* checkpoints (older versions took one of the line a
//     restore left, right after the restored point; it was never progress,
//     and checkpoints made that way still exist). A
//     checkpoint made by hand, by an upgrade (before-upgrade-*) or any other
//     way counts. The creation state ("reset") is the tip only of a lane with
//     no checkpoint and no transaction;
//  3. when the branch records its transactions (recorded), they are known for
//     every line of the lane; otherwise a transaction may follow `from`
//     unseen, and the line forks as before.
// Everything else forks a new lane as before. Line 0 is always a lane of its
// own, and a lane keeps the number of the first line drawn on it, so numbers
// are never renumbered (a lane may be line 3 beside line 0).
//
// What the API cannot say: the restore time, whether the data had changed since
// `from` (the automatic checkpoint holds that), and whether a restore was made
// to a transaction (a line made that way only names the checkpoint before it).
export function mergeLines(input: LayoutInput): Map<number, number> {
  const lines = [...input.lines].sort((a, b) => a.line - b.line);
  const cps = sortedCheckpoints(input.checkpoints);
  const rank = new Map(cps.map((c, i) => [c.name, i + 1]));
  const lane = new Map<number, number>();
  for (const l of lines) {
    lane.set(l.line, l.line);
    if (l.line === 0 || l.from === "" || l.parent === null) continue;
    const target = lane.get(l.parent);
    if (target === undefined) continue;
    const at = l.from === "reset" ? 0 : rank.get(l.from);
    const origin = cps.find((c) => c.name === l.from);
    if (at === undefined || (l.from !== "reset" && origin?.line !== l.parent)) continue;
    const members = lines.filter((m) => m.line < l.line && lane.get(m.line) === target).map((m) => m.line);
    if (input.recorded && members.some((m) => input.events?.[m] === undefined)) continue;
    const since = origin ? ms(origin.created_at) : -Infinity;
    const later =
      cps.some((c) => members.includes(c.line) && !isRestoreCopy(c) && rank.get(c.name)! > at) ||
      members.some((m) => (input.events?.[m] ?? []).some((x) => ms(x.time) > since));
    if (!later) lane.set(l.line, target);
  }
  return lane;
}

const sortedCheckpoints = (list: Checkpoint[]) => [...list].sort((a, b) => ms(a.created_at) - ms(b.created_at) || (a.name < b.name ? -1 : 1));

// layoutGraph lays the history out: one lane per line that did not continue
// another (mergeLines holds the exact rule, written once), the reset point and
// the checkpoints left to right in time order, a fork for every line that
// started somewhere in the middle of a lane, and "now" at the right end of the
// lane the branch runs on. A line that continues a lane has no fork and no
// label of its own: its nodes sit on that lane after the ones already there,
// and the lane runs on to now. Every node carries both its real line (for the
// API) and the line of its lane (for what is shown). The head ring stays on the
// checkpoint the data descends from.
export function layoutGraph(input: LayoutInput): GLayout {
  const lines = [...input.lines].sort((a, b) => a.line - b.line);
  const merged = mergeLines(input);
  const roots = [...new Set(lines.map((l) => merged.get(l.line)!))];
  const laneOfRoot = new Map(roots.map((r, i) => [r, i]));
  const laneOf = new Map(lines.map((l) => [l.line, laneOfRoot.get(merged.get(l.line)!)!]));
  const cps = sortedCheckpoints(input.checkpoints);
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

  nodes.push({ id: nodeId("reset", "0"), kind: "reset", x: xOf(0), y: yOf(laneOf.get(0) ?? 0), lane: laneOf.get(0) ?? 0, line: 0, shown: 0, time: 0 });

  for (const l of lines) {
    const lane = laneOf.get(l.line)!;
    const y = yOf(lane);
    const first = firstOnLine.get(l.line);
    const last = lastOnLine.get(l.line);
    // Where the lane starts: line 0 at the reset point; others just before
    // their first checkpoint, or at the right end while they have none.
    const startX = l.line === 0 ? xOf(0) : first ? xOf(slot.get(first.name)!) - SLOT_W * 0.5 : xOf(lastSlot) - SLOT_W * 0.5;
    const endX = l.live ? xOf(lastSlot) : last ? xOf(slot.get(last.name)!) : startX;
    const shown = merged.get(l.line)!;
    const end = Math.max(endX, startX);
    const drawn = lanes.find((x) => x.lane === lane);
    if (drawn) {
      // This line continues the lane: the lane runs on.
      drawn.x2 = Math.max(drawn.x2, end);
      drawn.live ||= l.live;
    } else {
      lanes.push({ line: l.line, lane, y, x1: startX, x2: end, live: l.live });
    }
    if (l.live) now = { x: drawn ? drawn.x2 : end, y, lane };

    if (shown === l.line && l.line !== 0 && l.from !== "") {
      const parentLane = laneOf.get(l.parent ?? 0) ?? 0;
      const fromSlot = l.from === "reset" ? 0 : slot.get(l.from);
      if (fromSlot !== undefined) forks.push({ line: l.line, x1: xOf(fromSlot), y1: yOf(parentLane), x2: startX, y2: y });
    }

    for (const c of cps.filter((c) => c.line === l.line)) {
      nodes.push({ id: nodeId("checkpoint", c.name), kind: "checkpoint", x: xOf(slot.get(c.name)!), y, lane, line: l.line, shown, time: ms(c.created_at), checkpoint: c, head: c.name === input.head });
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
          nodes.push({ id: nodeId("tx", `${l.line}:${tx.gtid}`), kind: "tx", x, y, lane, line: l.line, shown, time: ms(tx.time), tx });
        }
      }
    }
  }

  nodes.sort((a, b) => a.lane - b.lane || a.x - b.x);
  return { width: xOf(lastSlot) + SLOT_W * 0.9, height: yOf(Math.max(roots.length - 1, 0)) + 46, lanes, forks, nodes, now, sampled };
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
  if (n.kind === "checkpoint") return `Checkpoint ${n.checkpoint!.name}, line ${n.shown}`;
  return `Transaction ${n.tx!.gtid.split(":").pop()}, line ${n.shown}`;
}
