import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Checkpoint, HistoryTx, Line } from "../api";
import { HistoryGraph } from "./historygraph";

const at = (min: number) => new Date(Date.UTC(2026, 10, 12, 10, min)).toISOString();
const cp = (name: string, min: number, line: number, extra: Partial<Checkpoint> = {}): Checkpoint => ({ name, created_at: at(min), size: 1, line, auto: false, current: false, ...extra });
const tx = (n: number, min: number): HistoryTx => ({ gtid: `u:${n}`, time: at(min), sql: `insert ${n}`, truncated: false, tables: [], inserted: 1, updated: 0, deleted: 0, ddl: false, file: "f" });
const lines: Line[] = [
  { line: 0, live: false, parent: null, from: "" },
  { line: 1, live: true, parent: 0, from: "c1" },
];
const cps = [cp("c1", 0, 0), cp("c2", 3, 0), cp("before-restore-20261112-100500", 5, 0, { auto: true }), cp("a-very-long-checkpoint-name", 8, 1, { current: true })];
const html = (over: Partial<Parameters<typeof HistoryGraph>[0]> = {}) =>
  renderToStaticMarkup(<HistoryGraph branch="x" lines={lines} checkpoints={cps} head="a-very-long-checkpoint-name" selected={null} onSelect={() => {}} {...over} />);

describe("HistoryGraph", () => {
  it("labels the picture and every lane", () => {
    const h = html();
    expect(h).toContain('aria-label="History of x"');
    expect(h).toContain("line 0");
    expect(h).toContain("line 1");
  });
  it("draws a checkpoint as a button a keyboard and a screen reader can reach, with long names cut short", () => {
    const h = html();
    expect(h).toContain('role="button"');
    expect(h).toContain('aria-label="Checkpoint c1, line 0"');
    expect(h).toContain("a-very-long-…");
    expect(h).toContain("<title>a-very-long-checkpoint-name</title>");
  });
  it("puts exactly one node in the tab order: the head, or the selected one", () => {
    expect(html().match(/tabindex="0"/g)).toHaveLength(1);
    expect(html()).toMatch(/tabindex="0"[^>]*aria-label="Checkpoint a-very-long-checkpoint-name/);
    const h = html({ selected: "checkpoint:c1" });
    expect(h).toMatch(/tabindex="0"[^>]*aria-label="Checkpoint c1, line 0"[^>]*aria-pressed="true"/);
  });
  it("shows a restore as a branch, the head, and where the branch is now", () => {
    const h = html();
    expect(h).toContain("hg-fork");
    expect(h).toContain("hg-head");
    expect(h).toContain(">now<");
    expect(h).toContain("auto");
  });
  it("adds recorded transactions as dots when it has them, and says when a busy stretch was sampled", () => {
    expect(html()).not.toContain("hg-dot");
    const h = html({ events: { 0: [tx(1, 2), tx(2, 3)] } });
    expect(h.match(/hg-dot/g)).toHaveLength(2);
    expect(h).toContain('aria-label="Transaction 1, line 0"');
    expect(h).toContain("<title>insert 1</title>");
    expect(h).not.toContain("hg-note");
    const busy = Array.from({ length: 80 }, (_, i) => tx(i + 1, 1));
    expect(html({ events: { 0: busy } })).toContain("Busy stretches");
  });
});

describe("HistoryGraph: a restore of the tip", () => {
  const tipLines: Line[] = [
    { line: 0, live: false, parent: null, from: "" },
    { line: 1, live: true, parent: 0, from: "c2" },
  ];
  const tipCps = [cp("c1", 0, 0), cp("c2", 10, 0), cp("before-restore-1", 20, 0, { auto: true }), cp("d1", 30, 1, { current: true })];
  const tip = (over: Partial<Parameters<typeof HistoryGraph>[0]> = {}) =>
    renderToStaticMarkup(<HistoryGraph branch="x" lines={tipLines} checkpoints={tipCps} head="d1" selected={null} onSelect={() => {}} {...over} />);

  it("draws one lane with no fork and no label for the continued line, and tells the lane a node is on", () => {
    const h = tip();
    expect(h).not.toContain("hg-fork");
    expect(h).not.toContain("line 1");
    expect(h.match(/hg-lane /g)).toHaveLength(1);
    expect(h).toContain('aria-label="Checkpoint d1, line 0"');
    expect(h).toContain(">now<");
    expect(h).toContain("hg-head");
    expect(h).toMatch(/hg-checkpoint hg-c0 [^"]*"[^>]*><title>d1<\/title>/);
  });
  it("forks again when it is told the branch records transactions it cannot see", () => {
    const h = tip({ recorded: true });
    expect(h).toContain("hg-fork");
    expect(h).toContain("line 1");
  });
});

describe("HistoryGraph: enlarged", () => {
  it("scales the picture, keeping its proportions", () => {
    const w = (h: string) => Number(/<svg width="([\d.]+)" height="([\d.]+)" viewBox="0 0 ([\d.]+) ([\d.]+)"/.exec(h)![1]);
    const full = html();
    const big = html({ scale: 1.5 });
    expect(w(big)).toBeCloseTo(w(full) * 1.5);
    expect(big).toMatch(/viewBox="0 0 [\d.]+ [\d.]+"/);
    expect(big).toContain('class="hg big"');
    expect(full).not.toContain("hg big");
  });
});
