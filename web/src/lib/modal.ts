// What a modal dialog needs that does not depend on a browser: the keys that
// close it and keep focus inside, the backdrop click, and the page scroll lock.
// Tested with plain objects; components/modal.tsx wires them to the DOM.

// FOCUSABLE selects what the Tab key can reach (the roving-tabindex nodes of
// the history graph are reached with the arrows, so tabindex -1 is left out).
export const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

type KeyLike = { key: string; shiftKey: boolean; preventDefault: () => void };

// modalKey: Escape closes; Tab and Shift+Tab wrap around the focusable items
// of the dialog. active is the element that has focus now.
export function modalKey<T extends { focus: () => void }>(e: KeyLike, onClose: () => void, items: T[], active: unknown): void {
  if (e.key === "Escape") {
    e.preventDefault();
    onClose();
    return;
  }
  if (e.key !== "Tab") return;
  if (items.length === 0) return e.preventDefault();
  const first = items[0];
  const last = items[items.length - 1];
  const inside = items.includes(active as T);
  if (e.shiftKey && (!inside || active === first)) {
    e.preventDefault();
    last.focus();
  } else if (!e.shiftKey && (!inside || active === last)) {
    e.preventDefault();
    first.focus();
  }
}

type PointerLike = { target: unknown; currentTarget: unknown };

// backdropClose: a click on the backdrop closes the dialog, but not a click
// inside it, and not a drag (a text selection) that began inside and let go
// on the backdrop.
export function backdropClose(onClose: () => void) {
  let began = false;
  return {
    onMouseDown: (e: PointerLike) => {
      began = e.target === e.currentTarget;
    },
    onClick: (e: PointerLike) => {
      const ok = began && e.target === e.currentTarget;
      began = false;
      if (ok) onClose();
    },
  };
}

let locks = 0;
let before = "";

// lockScroll stops the page behind a dialog from scrolling and returns the
// function that gives it back. Locks nest: the page is released by the last.
export function lockScroll(root: { style: { overflow: string } }): () => void {
  if (locks++ === 0) {
    before = root.style.overflow;
    root.style.overflow = "hidden";
  }
  let released = false;
  return () => {
    if (released) return;
    released = true;
    if (--locks === 0) root.style.overflow = before;
  };
}
