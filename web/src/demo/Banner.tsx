// The strip across the top of the demo: what it is, who you are in it, and the
// way back to the real site.
import { useEffect, useRef, useState } from "react";
import { language, t } from "../lib/i18n";
import { interestUrl, tryLocallyUrl } from "./tryurl";

export type Role = "admin" | "developer";

export function Banner({ role, onSwitch, home }: { role: Role | null; onSwitch: (r: Role) => void; home: string }) {
  const [current, setCurrent] = useState(role);
  // The banner may wrap on a narrow screen: the page below it moves down by its real height.
  const bar = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = bar.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const sync = () => document.documentElement.style.setProperty("--demo-bar", `${el.offsetHeight}px`);
    const ro = new ResizeObserver(sync);
    ro.observe(el);
    sync();
    return () => ro.disconnect();
  }, []);
  const pick = (r: Role) => {
    setCurrent(r);
    onSwitch(r);
  };
  return (
    <div ref={bar} className="demo-banner" role="region" aria-label="Demo">
      <span className="demo-tag">Demo</span>
      <span className="demo-note">{t("Nothing here is real. Data resets when you reload.")}</span>
      <span className="demo-spacer" />
      <span className="demo-as">
        <span>View as</span>
        {(["admin", "developer"] as const).map((r) => (
          <button key={r} type="button" aria-pressed={current === r} className={current === r ? "on" : ""} onClick={() => pick(r)}>
            {r === "admin" ? "Admin console" : "Developer page"}
          </button>
        ))}
      </span>
      <a className="demo-try" href={tryLocallyUrl(language())}>
        {t("Run it on your own machine")}
      </a>
      <a className="demo-interest" href={interestUrl(language())}>
        {t("Interested in Team / Business?")}
      </a>
      <a className="demo-home" href={home}>
        Back to db-branch.com
      </a>
    </div>
  );
}
