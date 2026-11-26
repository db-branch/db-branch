// The admin console's navigation as data: the sidebar tree (pages, then a
// collapsible Modules group with one collapsible entry per module), which
// route a URL means, old URLs that moved, and the remembered open/closed
// state. No DOM access here, so it is testable; App.tsx draws it.
import type { ModulePage } from "../api";
import { moduleLabel } from "./modules";

// The admin console lives under /admin: /admin/<page>, /admin/modules[/<module>],
// /admin/m/<module><path>. A branch has a page of its own: /admin/branches/<name>.
export const ADMIN = "/admin";
export const NAV_KEY = "db-branch.nav.v1";

export type NavItem = { id: string; label: string; href?: string; children?: NavItem[] };

export type Route =
  | { kind: "page"; page: string }
  | { kind: "branch"; name: string }
  | { kind: "base"; name: string }
  | { kind: "overview" }
  | { kind: "module"; name: string }
  | { kind: "modpage"; module: string; path: string }
  | { kind: "pending" };

export type NavContext = {
  /** The top-level pages this user may see, in order. */
  pages: string[];
  /** May see Settings (and so the modules' overview and settings). */
  canSettings: boolean;
  /** Enabled modules; undefined until loaded (or when the user may not list them). */
  enabled?: string[];
  /** Module pages this user may open; undefined until loaded. */
  modPages?: ModulePage[];
};

const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

export function buildNav(ctx: NavContext): NavItem[] {
  const items: NavItem[] = ctx.pages.map((p) => ({ id: `page:${p}`, label: cap(p), href: `${ADMIN}/${p}` }));
  const pages = ctx.modPages ?? [];
  const names = Array.from(new Set([...(ctx.canSettings ? (ctx.enabled ?? []) : []), ...pages.map((p) => p.module)])).sort();
  const modules: NavItem[] = names.map((name) => ({
    id: `mod:${name}`,
    label: moduleLabel(name),
    children: [
      ...(ctx.canSettings ? [{ id: `modset:${name}`, label: "Settings", href: `${ADMIN}/modules/${encodeURIComponent(name)}` }] : []),
      ...pages.filter((p) => p.module === name).map((p) => ({ id: `modpage:${name}${p.path}`, label: p.title, href: `${ADMIN}/m/${name}${p.path}` })),
    ],
  }));
  if (ctx.canSettings) modules.unshift({ id: "overview", label: "Overview", href: `${ADMIN}/modules` });
  if (modules.length > 0) items.push({ id: "modules", label: "Modules", children: modules });
  return items;
}

export function resolveRoute(pathname: string, ctx: NavContext): Route {
  const parts = pathname.slice(ADMIN.length).split("/").filter(Boolean);
  const [head, second] = parts;
  if (head === "m") {
    if (!ctx.modPages) return { kind: "pending" };
    const hit = ctx.modPages.find((p) => pathname === `${ADMIN}/m/${p.module}${p.path}`);
    if (hit) return { kind: "modpage", module: hit.module, path: hit.path };
  } else if (head === "modules" && ctx.canSettings) {
    const name = second ? decodeURIComponent(second) : "";
    return name && (!ctx.enabled || ctx.enabled.includes(name)) ? { kind: "module", name } : { kind: "overview" };
  } else if (head === "branches" && second && ctx.pages.includes(head)) {
    return { kind: "branch", name: decodeURIComponent(second) };
  } else if (head === "bases" && second && ctx.pages.includes(head)) {
    return { kind: "base", name: decodeURIComponent(second) };
  } else if (head && ctx.pages.includes(head)) {
    return { kind: "page", page: head };
  }
  return { kind: "page", page: "branches" };
}

/** The id of the sidebar entry that is current. */
export function activeId(r: Route): string {
  switch (r.kind) {
    case "page":
      return `page:${r.page}`;
    case "branch":
      return "page:branches";
    case "base":
      return "page:bases";
    case "overview":
      return "overview";
    case "module":
      return `modset:${r.name}`;
    case "modpage":
      return `modpage:${r.module}${r.path}`;
    default:
      return "";
  }
}

/** The groups that must be open to show the current entry. */
export function ancestorsOf(r: Route): string[] {
  if (r.kind === "overview") return ["modules"];
  if (r.kind === "module") return ["modules", `mod:${r.name}`];
  if (r.kind === "modpage") return ["modules", `mod:${r.module}`];
  return [];
}

// Module tabs used to live in Settings (/admin/settings/modules,
// /admin/settings/<module>); they moved to /admin/modules[/<module>].
const settingsTabs = new Set(["general", "tokens", "updates"]);
export function legacyRedirect(pathname: string): string | null {
  const prefix = `${ADMIN}/settings/`;
  if (!pathname.startsWith(prefix)) return null;
  const tab = pathname.slice(prefix.length).split("/")[0];
  if (!tab || settingsTabs.has(decodeURIComponent(tab))) return null;
  return tab === "modules" ? `${ADMIN}/modules` : `${ADMIN}/modules/${tab}`;
}

// When the paid auth module provides its own Users page, /admin/users forwards
// there so core and module do not show two separate user lists.
export function usersRedirect(pathname: string, modPages?: ModulePage[]): string | null {
  if (pathname !== `${ADMIN}/users` && pathname !== `${ADMIN}/users/`) return null;
  const authPage = modPages?.find((p) => p.module === "auth");
  return authPage ? `${ADMIN}/m/auth${authPage.path}` : null;
}

export type Store = { getItem(k: string): string | null; setItem(k: string, v: string): void };
export type OpenState = Record<string, boolean>;

function browserStorage(): Store | undefined {
  try {
    return localStorage;
  } catch {
    return undefined;
  }
}

export function loadOpen(store: Store | undefined = browserStorage()): OpenState {
  try {
    const v = JSON.parse(store?.getItem(NAV_KEY) ?? "{}");
    return Object.fromEntries(Object.entries(v && typeof v === "object" ? v : {}).filter(([, x]) => x === true || x === false)) as OpenState;
  } catch {
    return {};
  }
}

export function saveOpen(open: OpenState, store: Store | undefined = browserStorage()): void {
  try {
    store?.setItem(NAV_KEY, JSON.stringify(open));
  } catch {
    // Remembering is a convenience; the menu works without it.
  }
}

/** Opens the given groups, leaving every other choice as it is. */
export function expandFor(open: OpenState, ids: string[]): OpenState {
  return ids.every((id) => open[id]) ? open : { ...open, ...Object.fromEntries(ids.map((id) => [id, true])) };
}

/** Reconciles sidebar open state on route change, collapsing Modules on mobile non-module routes. */
export function reconcileOpen(open: OpenState, ids: string[], collapseModules: boolean): OpenState {
  if (ids.length === 0 && collapseModules && open.modules) return { ...open, modules: false };
  return expandFor(open, ids);
}
