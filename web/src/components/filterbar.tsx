import type { ReactNode } from "react";
import { activeCount, FilterValues } from "../lib/filter";

export type Facet = { key: string; label: string; options: string[] };

// FilterBar is the search box and the filters above a list. The count appears
// only while something is set.
export function FilterBar({
  values,
  onChange,
  facets,
  shown,
  total,
  extra,
  extraActive = false,
  onClearExtra,
}: {
  values: FilterValues;
  onChange: (v: FilterValues) => void;
  facets: Facet[];
  shown: number;
  total: number;
  extra?: ReactNode;
  extraActive?: boolean;
  onClearExtra?: () => void;
}): ReactNode {
  const active = activeCount(values) > 0 || extraActive;
  return (
    <div className="filterbar" role="search">
      <input type="search" value={values.q ?? ""} placeholder="Search" aria-label="Search" onChange={(e) => onChange({ ...values, q: e.target.value })} />
      {facets
        .filter((f) => f.options.length > 0)
        .map((f) => (
          <select key={f.key} value={values[f.key] ?? ""} aria-label={f.label} className={values[f.key] ? "on" : ""} onChange={(e) => onChange({ ...values, [f.key]: e.target.value })}>
            <option value="">All {f.label.toLowerCase()}</option>
            {f.options.map((o) => (
              <option key={o} value={o}>
                {o}
              </option>
            ))}
          </select>
        ))}
      {extra}
      {active && (
        <>
          <span className="muted small mono">
            {shown} / {total}
          </span>
          <button
            type="button"
            className="text-button"
            onClick={() => {
              onChange(Object.fromEntries(Object.keys(values).map((k) => [k, ""])));
              onClearExtra?.();
            }}
          >
            Clear
          </button>
        </>
      )}
    </div>
  );
}
