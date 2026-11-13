import { ReactNode, RefObject, useEffect, useId, useMemo, useRef } from "react";
import { createPortal } from "react-dom";
import { backdropClose, FOCUSABLE, lockScroll, modalKey } from "../lib/modal";
import { t } from "../lib/i18n";

// Modal is a large dialog over the page: role="dialog" and aria-modal, named by
// its title. Focus moves into it when it opens and goes back to returnFocus
// (the button that opened it) when it closes; Tab stays inside; Escape, the
// Close button and a click on the backdrop close it; the page behind does not
// scroll. It is drawn on the body, above any dialog or drawer that holds the
// button, so a confirmation inside it is never covered.
export function Modal({ title, onClose, returnFocus, children }: { title: string; onClose: () => void; returnFocus?: RefObject<HTMLElement | null>; children: ReactNode }) {
  const id = useId();
  const box = useRef<HTMLDivElement>(null);
  const closing = useRef(onClose);
  closing.current = onClose;
  const backdrop = useMemo(() => backdropClose(() => closing.current()), []);
  const keys = (e: { key: string; shiftKey: boolean; preventDefault: () => void }) => modalKey(e, () => closing.current(), [...(box.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? [])], document.activeElement);

  useEffect(() => {
    const unlock = lockScroll(document.documentElement);
    box.current?.focus();
    // Focus can fall to the page when the control that had it goes away (a
    // confirmation that was answered): keys must still reach the dialog.
    const outside = (e: KeyboardEvent) => box.current && !box.current.contains(e.target as Node) && keys(e);
    document.addEventListener("keydown", outside);
    return () => {
      document.removeEventListener("keydown", outside);
      unlock();
      returnFocus?.current?.focus();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const modal = (
    <div
      className="gm-scrim"
      onMouseDown={(e) => (e.stopPropagation(), backdrop.onMouseDown(e))}
      onClick={(e) => (e.stopPropagation(), backdrop.onClick(e))}
    >
      <div
        className="gm"
        role="dialog"
        aria-modal="true"
        aria-labelledby={id}
        tabIndex={-1}
        ref={box}
        onKeyDown={(e) => {
          e.stopPropagation();
          keys(e);
        }}
      >
        <header className="gm-head">
          <h2 id={id}>{title}</h2>
          <button type="button" onClick={onClose}>
            {t("Close")}
          </button>
        </header>
        <div className="gm-body">{children}</div>
      </div>
    </div>
  );
  return typeof document === "undefined" ? modal : createPortal(modal, document.body);
}
