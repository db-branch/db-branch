import { MouseEvent } from "react";
import { Chevron } from "./scopeform";
import { NavItem, OpenState } from "../lib/nav";
import { t } from "../lib/i18n";

// The sidebar tree: links, and groups that open and close. Labels of the
// fixed entries are English words (t() leaves them alone); a module page's
// title comes from the module.
export function SideNav({
  items,
  active,
  open,
  onToggle,
  onGo,
}: {
  items: NavItem[];
  active: string;
  open: OpenState;
  onToggle: (id: string) => void;
  onGo: (href: string) => void;
}) {
  return (
    <>
      {items.map((it) => {
        if (!it.children) {
          const on = it.id === active;
          return (
            <a
              key={it.id}
              href={it.href}
              className={on ? "on" : ""}
              aria-current={on ? "page" : undefined}
              onClick={(e: MouseEvent) => {
                if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
                e.preventDefault();
                onGo(it.href!);
              }}
            >
              {it.id.startsWith("modpage:") ? it.label : t(it.label)}
            </a>
          );
        }
        const isOpen = !!open[it.id];
        const holdsActive = hasActive(it, active);
        return (
          <div key={it.id} className="nav-group">
            <button type="button" className={`nav-toggle${holdsActive ? " has-on" : ""}`} aria-expanded={isOpen} aria-controls={`nav-${it.id}`} onClick={() => onToggle(it.id)}>
              <Chevron />
              {it.id.startsWith("mod:") ? it.label : t(it.label)}
            </button>
            <div id={`nav-${it.id}`} className="nav-children" hidden={!isOpen}>
              <SideNav items={it.children} active={active} open={open} onToggle={onToggle} onGo={onGo} />
            </div>
          </div>
        );
      })}
    </>
  );
}

function hasActive(it: NavItem, active: string): boolean {
  return it.id === active || !!it.children?.some((c) => hasActive(c, active));
}
