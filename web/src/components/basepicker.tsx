import { KeyboardEvent, useEffect, useMemo, useRef, useState } from "react";
import { arrange, Choice, EngineFilter, matches, moveSelection } from "../lib/bases";
import { relative } from "../lib/format";
import { t } from "../lib/i18n";
import { Engine } from "./marks";

const engines: [EngineFilter, string][] = [
  ["all", "All"],
  ["mysql", "MySQL"],
  ["postgres", "PostgreSQL"],
];

// BasePicker: search, engine chips and a keyboard-driven list of bases.
// `/` focuses the search; arrows move; Enter picks and moves on (onPicked).
export function BasePicker({ choices, recent, value, onChange, onPicked }: { choices: Choice[]; recent: string[]; value: string; onChange: (name: string) => void; onPicked: () => void }) {
  const [query, setQuery] = useState("");
  const [engine, setEngine] = useState<EngineFilter>("all");
  const search = useRef<HTMLInputElement>(null);
  const list = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const onKey = (e: globalThis.KeyboardEvent) => {
      const el = e.target as HTMLElement;
      if (e.key === "/" && !e.metaKey && !e.ctrlKey && !/^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName) && !el.isContentEditable) {
        e.preventDefault();
        search.current?.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const shown = useMemo(() => arrange(choices.filter((c) => matches(c, query, engine)), recent), [choices, query, engine, recent]);
  const names = shown.map((c) => c.name);
  // Bases used lately come first; a hairline marks where the rest begins.
  const recentCount = !query.trim() && engine === "all" ? shown.filter((c) => recent.includes(c.name)).length : 0;

  useEffect(() => {
    // Scroll the list itself (not the page) so the selected row is in view.
    const box = list.current;
    const el = box?.querySelector<HTMLElement>('[aria-selected="true"]');
    if (!box || !el) return;
    if (el.offsetTop < box.scrollTop) box.scrollTop = el.offsetTop;
    else if (el.offsetTop + el.offsetHeight > box.scrollTop + box.clientHeight) box.scrollTop = el.offsetTop + el.offsetHeight - box.clientHeight;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [value, names.join(",")]);

  const onKeyDown = (e: KeyboardEvent) => {
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const next = moveSelection(names, names.includes(value) ? value : "", e.key === "ArrowDown" ? 1 : -1);
      onChange(next);
      (list.current?.querySelector(`[data-name="${CSS.escape(next)}"]`) as HTMLElement | null)?.focus();
    } else if (e.key === "Enter" && names.length > 0) {
      e.preventDefault();
      if (!names.includes(value)) onChange(names[0]);
      onPicked();
    }
  };

  return (
    <div className="base-picker" data-tour="base" onKeyDown={onKeyDown}>
      <div className="base-picker-bar">
        <input
          ref={search}
          type="search"
          className="base-search"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder={t("Search by name, engine or words  ( / )")}
          aria-label={t("Search bases")}
        />
        <div className="segmented small" role="group" aria-label="Engine">
          {engines.map(([id, label]) => (
            <button type="button" key={id} aria-pressed={engine === id} className={engine === id ? "on" : ""} onClick={() => setEngine(id)}>
              {label}
            </button>
          ))}
        </div>
      </div>
      <div className="base-list">
        <div className="base-choices" role="listbox" aria-label="Base" ref={list}>
          {shown.length === 0 && <p className="muted base-empty">{t("No base matches. Try fewer words or another engine.")}</p>}
          {shown.map((c, i) => (
            <button
              type="button"
              key={c.name}
              role="option"
              data-name={c.name}
              aria-selected={c.name === value}
              tabIndex={c.name === value || (!names.includes(value) && i === 0) ? 0 : -1}
              className={`base-row ${c.name === value ? "on" : ""} ${recentCount > 0 && i === recentCount ? "after-recent" : ""}`}
              title={c.description ? `${c.name}: ${c.description}` : c.name}
              onClick={() => (onChange(c.name), onPicked())}
            >
              <span className="mono strong base-ident">{c.name}</span>
              <Engine runtime={c.runtime} />
              <span className="base-blurb muted">
                {c.masked && (
                  <span className="badge on" title={t("This Snapshot was made from masked data.")}>
                    masked
                  </span>
                )}
                {c.description || t("No description.")}
              </span>
              <span className="muted small nowrap base-age">{c.snapshotAt ? relative(c.snapshotAt) : ""}</span>
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}
