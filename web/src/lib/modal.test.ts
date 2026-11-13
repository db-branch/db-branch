import { describe, expect, it, vi } from "vitest";
import { backdropClose, lockScroll, modalKey } from "./modal";

const key = (k: string, shift = false) => ({ key: k, shiftKey: shift, preventDefault: vi.fn() });
const item = () => ({ focus: vi.fn() });

describe("modalKey", () => {
  it("closes on Escape", () => {
    const close = vi.fn();
    const e = key("Escape");
    modalKey(e, close, [item()], null);
    expect(close).toHaveBeenCalledOnce();
    expect(e.preventDefault).toHaveBeenCalled();
  });
  it("keeps Tab inside: from the last to the first, and Shift+Tab from the first to the last", () => {
    const [a, b, c] = [item(), item(), item()];
    const fwd = key("Tab");
    modalKey(fwd, vi.fn(), [a, b, c], c);
    expect(a.focus).toHaveBeenCalledOnce();
    expect(fwd.preventDefault).toHaveBeenCalled();
    const back = key("Tab", true);
    modalKey(back, vi.fn(), [a, b, c], a);
    expect(c.focus).toHaveBeenCalledOnce();
    expect(back.preventDefault).toHaveBeenCalled();
  });
  it("lets Tab move on inside, and pulls focus that is outside or on the dialog itself in", () => {
    const [a, b, c] = [item(), item(), item()];
    const inside = key("Tab");
    modalKey(inside, vi.fn(), [a, b, c], b);
    expect(inside.preventDefault).not.toHaveBeenCalled();
    const outside = key("Tab");
    modalKey(outside, vi.fn(), [a, b, c], {});
    expect(a.focus).toHaveBeenCalledOnce();
    const outsideBack = key("Tab", true);
    modalKey(outsideBack, vi.fn(), [a, b, c], null);
    expect(c.focus).toHaveBeenCalledOnce();
  });
  it("keeps focus where it is when nothing in the dialog can take it, and ignores other keys", () => {
    const e = key("Tab");
    modalKey(e, vi.fn(), [], null);
    expect(e.preventDefault).toHaveBeenCalled();
    const other = key("a");
    const close = vi.fn();
    modalKey(other, close, [item()], null);
    expect(close).not.toHaveBeenCalled();
    expect(other.preventDefault).not.toHaveBeenCalled();
  });
});

describe("backdropClose", () => {
  const bd = {};
  const ev = (target: object) => ({ target, currentTarget: bd });
  it("closes on a click that starts and ends on the backdrop", () => {
    const close = vi.fn();
    const h = backdropClose(close);
    h.onMouseDown(ev(bd));
    h.onClick(ev(bd));
    expect(close).toHaveBeenCalledOnce();
  });
  it("does not close for a click inside the dialog, or a drag that began inside and ended on the backdrop", () => {
    const close = vi.fn();
    const h = backdropClose(close);
    h.onMouseDown(ev({}));
    h.onClick(ev({}));
    h.onMouseDown(ev({}));
    h.onClick(ev(bd));
    h.onClick(ev(bd));
    expect(close).not.toHaveBeenCalled();
  });
});

describe("lockScroll", () => {
  it("stops the page scrolling and gives it back, also when two dialogs are open", () => {
    const root = { style: { overflow: "auto" } };
    const first = lockScroll(root);
    expect(root.style.overflow).toBe("hidden");
    const second = lockScroll(root);
    first();
    expect(root.style.overflow).toBe("hidden");
    second();
    expect(root.style.overflow).toBe("auto");
    first();
    second();
    expect(root.style.overflow).toBe("auto");
  });
  it("restores an empty value too", () => {
    const root = { style: { overflow: "" } };
    lockScroll(root)();
    expect(root.style.overflow).toBe("");
  });
});
