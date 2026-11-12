// The pages paid modules serve inside the console (a frame each). In the demo
// there is no module to ask, so the frame shows a static page with sample rows.
// It is given to the frame as srcdoc: no request, and the page carries no script.
import { t } from "../lib/i18n";

const esc = (s: string) => s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]!);

type Table = { title: string; head: string[]; rows: string[][] };

function tables(now: number): Record<string, Table> {
  const ago = (min: number) => new Date(now - min * 60e3).toISOString().slice(0, 16).replace("T", " ") + " UTC";
  return {
    "auth/users": {
      title: "Users",
      head: ["User", "Role", "Team", "Last sign-in"],
      rows: [
        ["demo-admin", "admin", "—", ago(2)],
        ["alex@example.com", "developer", "Storefront", ago(11)],
        ["sam@example.com", "developer", "Storefront", ago(60 * 26)],
        ["ci-runner", "service", "—", ago(60 * 5)],
      ],
    },
    "audit/log": {
      title: "Audit log",
      head: ["Time", "User", "Action", "Target", "Outcome"],
      rows: [
        [ago(3), "alex@example.com", "branch.create", "alex-checkout-fix", "ok"],
        [ago(42), "demo-admin", "base.snapshot", "storefront", "ok"],
        [ago(95), "ci-runner", "branch.delete", "ci-pr-471", "ok"],
        [ago(60 * 7), "demo-admin", "setting.write", "core", "ok"],
        [ago(60 * 9), "demo-admin", "auth.login", "—", "ok"],
      ],
    },
  };
}

const CSS =
  ":root{color-scheme:light dark}body{margin:0;padding:24px 28px;font:14px/1.45 system-ui,sans-serif;background:Canvas;color:CanvasText}" +
  "h1{margin:0 0 4px;font-size:18px;font-weight:500}p{margin:0 0 16px;opacity:.65;font-size:13px}table{border-collapse:collapse;width:100%}" +
  "th,td{text-align:left;padding:9px 12px;border-bottom:1px solid color-mix(in srgb,CanvasText 16%,transparent)}th{font-size:11px;letter-spacing:.06em;text-transform:uppercase;opacity:.65;font-weight:500}" +
  "td:first-child{font-family:ui-monospace,monospace;font-size:13px}";

/** modulePage is the HTML shown for a module's page, or a short notice for a page the demo does not have. */
export function modulePage(module: string, path: string, now = Date.now()): string {
  const table = tables(now)[module + "/" + path.replace(/^\//, "")];
  const body = table
    ? `<h1>${esc(t(table.title))}</h1><p>${esc(t("This page is served by the {module} module on a real cluster. Here it shows sample data.", { module }))}</p>` +
      `<table><thead><tr>${table.head.map((h) => `<th>${esc(t(h))}</th>`).join("")}</tr></thead><tbody>` +
      table.rows.map((r) => `<tr>${r.map((c) => `<td>${esc(c)}</td>`).join("")}</tr>`).join("") +
      "</tbody></table>"
    : `<h1>${esc(module)}</h1><p>${esc(t("The demo has no sample for this page."))}</p>`;
  return `<!doctype html><meta charset="utf-8"><style>${CSS}</style>${body}`;
}
