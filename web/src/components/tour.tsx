// A small in-house product tour: a dimmed backdrop, a highlighted target and a
// popover. Esc skips, Enter and the arrow keys move, Tab stays inside.
import { CSSProperties, useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from "react";
import { placePopover, resolveSteps, TourStep } from "../lib/tour";
import { t } from "../lib/i18n";

export function TourCard({
  id,
  title,
  body,
  index,
  total,
  onNext,
  onBack,
  onSkip,
  style,
  cardRef,
}: {
  id: string;
  title: string;
  body: string;
  index: number;
  total: number;
  onNext: () => void;
  onBack: () => void;
  onSkip: () => void;
  style?: CSSProperties;
  cardRef?: React.Ref<HTMLDivElement>;
}) {
  const last = index === total - 1;
  return (
    <div className="tour-card" role="dialog" aria-modal="true" aria-labelledby={`${id}-title`} aria-describedby={`${id}-body`} tabIndex={-1} ref={cardRef} style={style}>
      <h2 id={`${id}-title`}>{title}</h2>
      <p id={`${id}-body`}>{body}</p>
      <div className="tour-foot">
        <span className="muted small tour-progress">{t("{n} / {total}", { n: index + 1, total })}</span>
        <span className="spacer" />
        {!last && (
          <button type="button" className="text-button" onClick={onSkip}>
            {t("Skip tour")}
          </button>
        )}
        {index > 0 && (
          <button type="button" onClick={onBack}>
            {t("Back")}
          </button>
        )}
        <button type="button" className="primary" onClick={last ? onSkip : onNext}>
          {last ? t("Done") : t("Next")}
        </button>
      </div>
    </div>
  );
}

const reduced = () => typeof matchMedia === "function" && matchMedia("(prefers-reduced-motion: reduce)").matches;
const PAD = 6;

// Tour shows the steps whose targets exist. onClose runs for skip, Done and Esc.
export function Tour({ steps, onClose }: { steps: TourStep[]; onClose: () => void }) {
  const id = useId();
  // Steps are fixed when the tour opens; a target that disappears later centres the popover.
  const [list] = useState(() => resolveSteps(steps, (s) => !!document.querySelector(s)));
  const [index, setIndex] = useState(0);
  const [, redraw] = useState(0);
  const card = useRef<HTMLDivElement>(null);
  const step = list[index];

  const next = useCallback(() => (index >= list.length - 1 ? onClose() : setIndex(index + 1)), [index, list.length, onClose]);
  const back = useCallback(() => setIndex(Math.max(0, index - 1)), [index]);

  useEffect(() => {
    if (list.length === 0) onClose();
  }, [list.length, onClose]);

  // Focus goes into the popover and returns to where it was.
  useEffect(() => {
    const before = document.activeElement as HTMLElement | null;
    return () => {
      if (before && before.isConnected) before.focus();
    };
  }, []);

  const target = step ? document.querySelector<HTMLElement>(step.anchor) : null;
  useEffect(() => {
    target?.scrollIntoView({ block: "center", behavior: reduced() ? "auto" : "smooth" });
    card.current?.focus();
  }, [index, target]);

  useEffect(() => {
    const again = () => redraw((n) => n + 1);
    window.addEventListener("resize", again);
    window.addEventListener("scroll", again, true);
    // The page keeps growing while it loads: measure again when the target or its parent changes size.
    const watch = typeof ResizeObserver === "function" ? new ResizeObserver(again) : null;
    if (target) watch?.observe(target);
    if (target?.parentElement) watch?.observe(target.parentElement);
    return () => {
      watch?.disconnect();
      window.removeEventListener("resize", again);
      window.removeEventListener("scroll", again, true);
    };
  }, [target]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const onButton = (e.target as HTMLElement | null)?.tagName === "BUTTON";
      const go = (fn: () => void) => (e.preventDefault(), e.stopPropagation(), fn());
      if (e.key === "Escape") go(onClose);
      else if (e.key === "ArrowRight" || e.key === "ArrowDown" || (e.key === "Enter" && !onButton)) go(next);
      else if (e.key === "ArrowLeft" || e.key === "ArrowUp") go(back);
      else if (e.key === "Tab" && card.current) {
        const items = [card.current, ...card.current.querySelectorAll<HTMLElement>("button")];
        const at = items.indexOf(document.activeElement as HTMLElement);
        const to = e.shiftKey ? (at <= 0 ? items.length - 1 : at - 1) : at === items.length - 1 ? 0 : at + 1;
        go(() => items[to].focus());
      }
    };
    document.addEventListener("keydown", onKey, true);
    return () => document.removeEventListener("keydown", onKey, true);
  }, [next, back, onClose]);

  const rect = target?.getBoundingClientRect() ?? null;
  const [size, setSize] = useState({ width: 340, height: 170 });
  useLayoutEffect(() => {
    const c = card.current;
    if (c && (c.offsetWidth !== size.width || c.offsetHeight !== size.height)) setSize({ width: c.offsetWidth, height: c.offsetHeight });
  });
  const at = useMemo(
    () => placePopover(rect, size, { width: window.innerWidth, height: window.innerHeight }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [rect?.top, rect?.left, rect?.bottom, rect?.right, size.width, size.height, window.innerWidth, window.innerHeight],
  );
  if (!step) return null;

  return (
    <div className="tour">
      {rect ? <div className="tour-hole" style={{ top: rect.top - PAD, left: rect.left - PAD, width: rect.width + 2 * PAD, height: rect.height + 2 * PAD }} /> : <div className="tour-dim" />}
      <TourCard id={id} title={step.title} body={step.body} index={index} total={list.length} onNext={next} onBack={back} onSkip={onClose} style={at} cardRef={card} />
    </div>
  );
}
