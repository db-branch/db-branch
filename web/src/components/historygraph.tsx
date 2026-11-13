import { KeyboardEvent, useEffect, useRef } from "react";
import type { Checkpoint, HistoryTx, Line } from "../api";
import { describe, GNode, layoutGraph, neighbour } from "../lib/historygraph";
import { t } from "../lib/i18n";

const LABEL = 13; // characters of a checkpoint's name drawn under its node

const shorten = (s: string) => (s.length > LABEL ? s.slice(0, LABEL - 1) + "…" : s);

// HistoryGraph draws a branch's history: one lane per line of history,
// checkpoints as nodes, a restore as a branch off the checkpoint it restored,
// and, when the history module is on and the person may read it, recorded
// transactions as small dots. Select a node by clicking it or with Enter or
// Space; the arrow keys move between nodes. The page shows what was selected.
export function HistoryGraph({
  branch,
  lines,
  checkpoints,
  head,
  events,
  recorded,
  scale = 1,
  selected,
  onSelect,
}: {
  branch: string;
  lines: Line[];
  checkpoints: Checkpoint[];
  head: string | null;
  events?: Record<number, HistoryTx[]>;
  // recorded: the branch records its transactions (see mergeLines).
  recorded?: boolean;
  // scale draws the whole picture larger (the enlarged view), keeping its proportions.
  scale?: number;
  selected: string | null;
  onSelect: (node: GNode) => void;
}) {
  const g = layoutGraph({ lines, checkpoints, head, events, recorded });
  const refs = useRef<Record<string, SVGGElement | null>>({});
  const scroller = useRef<HTMLDivElement>(null);
  // The one node in the tab order (the others are reached with the arrows).
  const focusable = g.nodes.find((n) => n.id === selected) ?? g.nodes.find((n) => n.head) ?? g.nodes[g.nodes.length - 1];

  useEffect(() => {
    // The newest part is on the right: show it first.
    const el = scroller.current;
    if (el) el.scrollLeft = el.scrollWidth;
  }, [g.width]);

  const key = (e: KeyboardEvent, n: GNode) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onSelect(n);
    } else if (e.key === "ArrowLeft" || e.key === "ArrowRight" || e.key === "ArrowUp" || e.key === "ArrowDown") {
      const next = neighbour(g.nodes, n, e.key);
      if (next) {
        e.preventDefault();
        refs.current[next.id]?.focus();
      }
    }
  };

  return (
    <div className={scale === 1 ? "hg" : "hg big"} ref={scroller}>
      <svg width={g.width * scale} height={g.height * scale} viewBox={`0 0 ${g.width} ${g.height}`} role="group" aria-label={`History of ${branch}`}>
        {g.lanes.map((l) => (
          <g key={`lane-${l.line}`} className={`hg-lane hg-c${l.line % 6}`}>
            <text x={8} y={l.y + 4} className="hg-lane-label">
              line {l.line}
            </text>
            <line x1={l.x1} x2={l.x2} y1={l.y} y2={l.y} className="hg-track" />
          </g>
        ))}
        {g.forks.map((f) => (
          <path key={`fork-${f.line}`} className={`hg-fork hg-c${f.line % 6}`} d={`M ${f.x1} ${f.y1} C ${f.x1 + 34} ${f.y1}, ${f.x2 - 34} ${f.y2}, ${f.x2} ${f.y2}`} fill="none" />
        ))}
        {g.nodes.map((n) => {
          const label = n.kind === "checkpoint" ? n.checkpoint!.name : n.kind === "reset" ? "creation" : "";
          return (
            <g
              key={n.id}
              ref={(el) => {
                refs.current[n.id] = el;
              }}
              role="button"
              tabIndex={n.id === focusable?.id ? 0 : -1}
              aria-label={describe(n)}
              aria-pressed={n.id === selected}
              className={`hg-node hg-${n.kind} hg-c${n.shown % 6} ${n.id === selected ? "selected" : ""} ${n.checkpoint?.auto ? "auto" : ""}`}
              onClick={() => onSelect(n)}
              onKeyDown={(e) => key(e, n)}
            >
              <title>{n.kind === "tx" ? n.tx!.sql || n.tx!.gtid : label}</title>
              {n.kind === "tx" ? <circle cx={n.x} cy={n.y} r={3.2} className="hg-dot" /> : null}
              {n.kind === "tx" ? <circle cx={n.x} cy={n.y} r={9} className="hg-hit" /> : null}
              {n.kind === "checkpoint" ? <circle cx={n.x} cy={n.y} r={n.checkpoint!.auto ? 5 : 6.5} className="hg-cp" /> : null}
              {n.kind === "reset" ? <rect x={n.x - 5} y={n.y - 5} width={10} height={10} className="hg-cp" /> : null}
              {n.head ? <circle cx={n.x} cy={n.y} r={11} className="hg-head" /> : null}
              <circle cx={n.x} cy={n.y} r={n.kind === "tx" ? 8 : 14} className="hg-focus" />
              {label ? (
                <text x={n.x} y={n.y + 25} textAnchor="middle" className="hg-name">
                  {shorten(label)}
                </text>
              ) : null}
            </g>
          );
        })}
        {g.now ? (
          <g className="hg-now" aria-hidden="true">
            <path d={`M ${g.now.x + 10} ${g.now.y - 5} l 9 5 l -9 5 z`} />
            <text x={g.now.x + 24} y={g.now.y + 4}>now</text>
          </g>
        ) : null}
      </svg>
      {g.sampled > 0 && <p className="muted small hg-note">{t("Busy stretches show a sample of their transactions.")}</p>}
    </div>
  );
}
