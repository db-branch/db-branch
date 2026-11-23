// The strip across the top of the demo: what it is, who you are in it, and the
// way back to the real site.
import { useState } from "react";
import { language, t } from "../lib/i18n";
import { tryLocallyUrl } from "./tryurl";

export type Role = "admin" | "developer";

export function Banner({ role, onSwitch, home }: { role: Role | null; onSwitch: (r: Role) => void; home: string }) {
  const [current, setCurrent] = useState(role);
  const pick = (r: Role) => {
    setCurrent(r);
    onSwitch(r);
  };
  return (
    <div className="demo-banner" role="region" aria-label="Demo">
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
      <a className="demo-home" href={home}>
        Back to db-branch.com
      </a>
    </div>
  );
}
