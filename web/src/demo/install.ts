// Turns the console into the static demo: before it starts, the browser APIs it
// talks to the agent with are replaced by a fake agent (backend.ts) that lives in
// this tab. Only imported by main.tsx in the demo build (--mode demo).
import { createElement } from "react";
import { createRoot } from "react-dom/client";
import { hooks } from "../lib/hooks";
import { t } from "../lib/i18n";
import { ADMIN } from "../lib/nav";
import { tourKey } from "../lib/tour";
import { DemoBackend } from "./backend";
import { Banner, Role } from "./Banner";
import { modulePage } from "./modulepages";
import { KEY, shouldRestore } from "./persist";
import { fakeFetch, makeEventSource, makeXHR } from "./shim";
import { DEVELOPER } from "./state";
import "./banner.css";

const HOME = "https://db-branch.com";

export function installDemo(): void {
  // The demo has no server: commands that name the host show an example one.
  (window as unknown as { dbbHost?: string }).dbbHost = "node1.example.com:7080";
  const nav = performance.getEntriesByType("navigation")[0] as PerformanceNavigationTiming | undefined;
  let saved: string | null = null;
  try {
    saved = sessionStorage.getItem(KEY);
  } catch {
    /* storage unavailable: no state across pages */
  }
  // Only a move inside the demo (a link, the switcher, back/forward) keeps the data; a typed or external address starts afresh.
  const inside = document.referrer.startsWith(location.origin) || nav?.type === "back_forward";
  const restore = inside && shouldRestore(saved, nav?.type);
  const backend = restore ? DemoBackend.load(saved) : new DemoBackend();
  backend.state.keep = false;
  if (!restore) {
    // A fresh visit starts signed in: the admin console as the admin, the rest as the developer, with the first-visit tour to come.
    backend.signIn(location.pathname === ADMIN || location.pathname.startsWith(ADMIN + "/") ? "admin" : "developer");
    try {
      localStorage.removeItem(tourKey(DEVELOPER.id));
    } catch {
      /* nothing to forget */
    }
  }
  const persist = () => {
    try {
      sessionStorage.setItem(KEY, backend.dump());
    } catch {
      /* storage unavailable */
    }
  };
  addEventListener("pagehide", persist);

  window.fetch = fakeFetch(backend, window.fetch.bind(window), location.origin);
  window.EventSource = makeEventSource(backend) as unknown as typeof EventSource;
  window.XMLHttpRequest = makeXHR(backend, location.origin, window.XMLHttpRequest) as unknown as typeof XMLHttpRequest;
  hooks.loginHint = () => t("Demo: any user name and password will do.");
  hooks.frame = (module, path) => ({ srcDoc: modulePage(module, path) });

  // The only files the console links to under /v1 are backups: hand out a clearly fake one.
  document.addEventListener("click", (e) => {
    const a = (e.target as Element | null)?.closest?.<HTMLAnchorElement>("a[href^='/v1/backups/']");
    if (!a) return;
    e.preventDefault();
    const name = decodeURIComponent(new URL(a.href).pathname.split("/").pop() ?? "backup");
    const url = URL.createObjectURL(new Blob(["This is a sample file from the db-branch demo. It holds no data.\n"], { type: "text/plain" }));
    const d = Object.assign(document.createElement("a"), { href: url, download: name });
    d.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  });

  const user = backend.state.user;
  const host = document.body.appendChild(document.createElement("div"));
  const switchTo = (r: Role) => {
    backend.signIn(r);
    persist();
    location.assign(r === "admin" ? ADMIN : "/");
  };
  createRoot(host).render(createElement(Banner, { role: user ? (user.admin ? "admin" : "developer") : null, onSwitch: switchTo, home: HOME }));
}
