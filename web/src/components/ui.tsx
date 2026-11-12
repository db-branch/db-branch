import { ReactNode, useEffect, useId, useRef, useState } from "react";
import { Icon } from "./marks";
import { t } from "../lib/i18n";

export function Drawer({ title, onClose, children, wide }: { title: string; onClose: () => void; children: ReactNode; wide?: boolean }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);
  return (
    <div className="scrim" onMouseDown={onClose}>
      <aside className={wide ? "drawer wide" : "drawer"} role="dialog" aria-label={title} onMouseDown={(e) => e.stopPropagation()}>
        <header className="drawer-head">
          <h2>{title}</h2>
          <button className="text-button" onClick={onClose}>
            {t("Close")}
          </button>
        </header>
        {children}
      </aside>
    </div>
  );
}

export function Dialog({ title, onClose, children }: { title: string; onClose: () => void; children: ReactNode }) {
  return (
    <div className="scrim center" onMouseDown={onClose}>
      <div className="dialog" role="dialog" aria-label={title} onMouseDown={(e) => e.stopPropagation()}>
        <h2>{title}</h2>
        {children}
      </div>
    </div>
  );
}

export type MenuItem = { label: string; onSelect: () => void; danger?: boolean; disabled?: boolean };

// Menu opens a list of actions. The list is fixed-positioned against the
// viewport (flipping up near the bottom), so a scrolling table cannot clip it.
export function Menu({ items, label }: { items: MenuItem[]; label: string }) {
  const [at, setAt] = useState<{ right: number; top?: number; bottom?: number } | null>(null);
  const ref = useRef<HTMLDivElement>(null);
  const open = at !== null;
  useEffect(() => {
    if (!open) return;
    const close = (e: Event) => {
      if (e.type === "mousedown" && ref.current?.contains(e.target as Node)) return;
      setAt(null);
    };
    const esc = (e: KeyboardEvent) => e.key === "Escape" && setAt(null);
    window.addEventListener("mousedown", close);
    window.addEventListener("scroll", close, true);
    window.addEventListener("resize", close);
    window.addEventListener("keydown", esc);
    return () => {
      window.removeEventListener("mousedown", close);
      window.removeEventListener("scroll", close, true);
      window.removeEventListener("resize", close);
      window.removeEventListener("keydown", esc);
    };
  }, [open]);
  const toggle = (e: { currentTarget: HTMLElement }) => {
    if (open) return setAt(null);
    const r = e.currentTarget.getBoundingClientRect();
    const right = Math.max(8, window.innerWidth - r.right);
    const roomBelow = window.innerHeight - r.bottom;
    setAt(roomBelow < items.length * 28 + 24 ? { right, bottom: window.innerHeight - r.top + 4 } : { right, top: r.bottom + 4 });
  };
  return (
    <div className="menu" ref={ref}>
      <button className="icon-button" aria-label={label} title={label} aria-expanded={open} onClick={toggle}>
        <Icon name="more" />
      </button>
      {at && (
        <ul className="menu-list" role="menu" style={{ position: "fixed", right: at.right, top: at.top, bottom: at.bottom, left: "auto" }}>
          {items.map((it) => (
            <li key={it.label}>
              <button
                role="menuitem"
                className={it.danger ? "danger" : ""}
                disabled={it.disabled}
                onClick={() => {
                  setAt(null);
                  it.onSelect();
                }}
              >
                {it.label}
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

export function Field({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return (
    <label className="field">
      <span className="field-label">{label}</span>
      {children}
      {hint && <span className="field-hint">{hint}</span>}
    </label>
  );
}

export function ErrorLine({ error }: { error: string | null }) {
  return error ? (
    <p className="error-line" role="alert">
      {error}
    </p>
  ) : null;
}

export function Empty({ line, action }: { line: string; action?: ReactNode }) {
  return (
    <div className="empty">
      <p>{line}</p>
      {action}
    </div>
  );
}

// Info is an "i" mark that explains something on hover or focus. The tip is
// fixed-positioned so scrolling containers do not clip it.
export function Info({ children, label = "More information" }: { children: ReactNode; label?: string }) {
  const [at, setAt] = useState<{ x: number; y: number } | null>(null);
  const id = useId();
  const show = (e: { currentTarget: HTMLElement }) => {
    const r = e.currentTarget.getBoundingClientRect();
    setAt({ x: Math.min(Math.max(r.left + r.width / 2, 150), window.innerWidth - 150), y: r.bottom + 6 });
  };
  return (
    <span
      className="info"
      tabIndex={0}
      role="button"
      aria-label={label}
      aria-describedby={at ? id : undefined}
      onMouseEnter={show}
      onFocus={show}
      onMouseLeave={() => setAt(null)}
      onBlur={() => setAt(null)}
      onKeyDown={(e) => e.key === "Escape" && setAt(null)}
    >
      i
      {at && (
        <span role="tooltip" id={id} className="info-tip" style={{ left: at.x, top: at.y }}>
          {children}
        </span>
      )}
    </span>
  );
}
