import { useState } from "react";

// Sparkline: one series, thin 2px line in the accent colour, a light area
// beneath, and on hover a vertical rule with the value. The label next to it
// names the series, so there is no legend.
export function Sparkline({
  values,
  times,
  max,
  format,
  label,
}: {
  values: number[];
  times: string[];
  max?: number;
  format: (v: number) => string;
  label: string;
}) {
  const [hover, setHover] = useState<number | null>(null);
  const w = 120;
  const h = 28;
  const top = Math.max(max ?? 0, ...values, 1e-9);
  const n = values.length;
  const x = (i: number) => (n <= 1 ? w : (i / (n - 1)) * w);
  const y = (v: number) => h - 2 - (Math.min(v, top) / top) * (h - 4);
  const line = values.map((v, i) => `${i ? "L" : "M"}${x(i).toFixed(1)},${y(v).toFixed(1)}`).join("");
  const area = n > 1 ? `${line}L${w},${h}L0,${h}Z` : "";
  const shown = hover ?? n - 1;
  return (
    <span className="spark" aria-label={`${label}: ${n ? format(values[n - 1]) : "no data"}`}>
      <svg
        viewBox={`0 0 ${w} ${h}`}
        width={w}
        height={h}
        role="img"
        onMouseMove={(e) => {
          const r = e.currentTarget.getBoundingClientRect();
          setHover(n ? Math.round(((e.clientX - r.left) / r.width) * (n - 1)) : null);
        }}
        onMouseLeave={() => setHover(null)}
      >
        {area && <path d={area} className="spark-area" />}
        {n > 0 && <path d={line} className="spark-line" />}
        {hover !== null && n > 0 && <line x1={x(hover)} x2={x(hover)} y1={0} y2={h} className="spark-rule" />}
      </svg>
      <span className="mono spark-value" title={hover !== null && times[hover] ? new Date(times[hover]).toLocaleTimeString() : undefined}>
        {n ? format(values[shown]) : "—"}
      </span>
    </span>
  );
}
