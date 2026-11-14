import { FormEvent, useCallback, useEffect, useState } from "react";
import { api, ApiError, can, isAdmin, LoginMethod, managesBases, Session, ssoReturn } from "./api";
import { AccountMenu } from "./components/account";
import { SideNav } from "./components/sidenav";
import { ErrorLine, Field } from "./components/ui";
import { hooks, moduleFrame } from "./lib/hooks";
import { chooseLanguage, language, remembered, setLanguage, t } from "./lib/i18n";
import { GoContext } from "./lib/gocontext";
import { disconnect, useLive } from "./lib/live";
import { activeId, ADMIN, ancestorsOf, buildNav, expandFor, legacyRedirect, loadOpen, resolveRoute, saveOpen } from "./lib/nav";
import { Bases } from "./pages/Bases";
import { BaseDetailPage } from "./pages/BaseDetailPage";
import { BranchDetail } from "./pages/BranchDetail";
import { Branches } from "./pages/Branches";
import { Cluster } from "./pages/Cluster";
import { Modules } from "./pages/Modules";
import { Settings } from "./pages/Settings";
import { Storage } from "./pages/Storage";

const pages = { branches: Branches, bases: Bases, storage: Storage, cluster: Cluster, settings: Settings } as const;
type Page = keyof typeof pages;

// Who sees each page: maintainers get branches, bases and the cluster;
// settings are for admins.
const pageAction: Record<Page, string> = { branches: "branch.read", bases: "base.read", storage: "cluster.read", cluster: "cluster.read", settings: "setting.write" };
const visiblePages = () => (Object.keys(pages) as Page[]).filter((p) => isAdmin() || can(pageAction[p]));

// The admin console lives under /admin (see lib/nav.ts); main.tsx imports it from here.
export { ADMIN };

export function App() {
  const [state, setState] = useState<"loading" | "login" | "in">("loading");
  const [session, setSession] = useState<Session | null>(null);
  const enter = (s: Session) => {
    setLanguage(chooseLanguage(s.language || remembered(), navigator.language));
    setSession(s);
    setState("in");
  };
  const [ssoError, setSsoError] = useState<string | undefined>();
  useEffect(() => {
    ssoReturn().then(({ session, error }) => {
      if (session) return enter(session);
      setSsoError(error);
      api.session().then(enter, () => setState("login"));
    });
  }, []);
  if (state === "loading") return null;
  if (state === "login" || !session)
    return <Login onDone={enter} defaultUser="admin" hint={t("Forgot the admin password? sudo db-branch admin reset-password")} initialError={ssoError} />;
  if (!managesBases()) return <NotAdmin session={session} onLogout={() => setState("login")} />;
  return (
    <Shell
      session={session}
      onLogout={() => {
        disconnect();
        setState("login");
      }}
    />
  );
}

// NotAdmin is shown to people without the admin role who open /admin.
function NotAdmin({ session, onLogout }: { session: Session; onLogout: () => void }) {
  return (
    <main className="login">
      <div className="login-box">
        <div className="wordmark">
          <Mark /> db-branch
        </div>
        <p>
          {t("Signed in as")} <span className="mono">{session.user.name}</span>
          {t(", who is not an administrator or maintainer.")}
        </p>
        <a className="button primary wide" href="/">
          {t("Go to my branches")}
        </a>
        <button className="text-button" onClick={() => api.logout().finally(onLogout)}>
          {t("Sign in as someone else")}
        </button>
      </div>
    </main>
  );
}

export function Login({
  onDone,
  defaultUser = "",
  hint,
  onCancel,
  initialError,
}: {
  onDone: (s: Session) => void;
  defaultUser?: string;
  hint?: string;
  onCancel?: () => void;
  initialError?: string;
}) {
  const [username, setUsername] = useState(defaultUser);
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(initialError ?? null);
  const [methods, setMethods] = useState<LoginMethod[]>([]);
  useEffect(() => {
    api.loginMethods().then(setMethods, () => {});
  }, []);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    try {
      onDone(await api.login(username.trim() || "admin", password));
    } catch (err) {
      setError(err instanceof ApiError && err.code === "invalid_credentials" ? t("Wrong password.") : (err as Error).message);
    }
  };
  return (
    <main className="login">
      <form className="login-box" onSubmit={submit}>
        <div className="wordmark">
          <Mark /> db-branch
        </div>
        <Field label={t("Email or user name")}>
          <input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" placeholder="you@example.com" />
        </Field>
        <Field label={t("Password")} hint={hint ?? hooks.loginHint?.()}>
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoFocus autoComplete="current-password" />
        </Field>
        <ErrorLine error={error} />
        <button className="primary wide" disabled={!password}>
          {t("Sign in")}
        </button>
        {methods.length > 0 && <div className="login-or">{t("or")}</div>}
        {methods.map((m) => (
          <a key={m.url} className="button wide" href={`${m.url}?return=${encodeURIComponent(location.pathname)}`}>
            {t(m.label)}
          </a>
        ))}
        {onCancel && (
          <button type="button" className="text-button" onClick={onCancel}>
            {t("Continue without signing in")}
          </button>
        )}
      </form>
    </main>
  );
}

// The mark: a trunk with one branch, drawn with the same hairline as the UI.
export function Mark() {
  return (
    <svg className="mark" width="18" height="18" viewBox="0 0 18 18" aria-hidden="true">
      <path d="M5 2v14M5 11c0-3 3-4 6-4h2" />
      <circle cx="13" cy="7" r="2" />
    </svg>
  );
}

// Shell is the signed-in console: the sidebar tree and the page the URL names.
// A module page is shown in a frame: /admin/m/<module><path>.
function Shell({ session, onLogout }: { session: Session; onLogout: () => void }) {
  const canSettings = isAdmin() || can(pageAction.settings);
  // Old module URLs under Settings moved to /admin/modules; fix the address before reading it.
  const sync = () => {
    const to = legacyRedirect(location.pathname);
    if (to) history.replaceState(null, "", to + location.search + location.hash);
    return location.pathname;
  };
  const [path, setPath] = useState(sync);
  const { data: caps } = useLive(api.capabilities, ["module"]);
  const { data: mods } = useLive(() => (canSettings ? api.modules() : Promise.resolve(undefined)), ["module"]);
  // A module's pages for those allowed its API (e.g. maintainers: auth's Users).
  const modPages = caps && (caps.pages ?? []).filter((p) => isAdmin() || can(`ext.${p.module}.read`));
  const enabled = canSettings ? mods?.items.filter((m) => m.wanted).map((m) => m.name) : [];
  const ctx = { pages: visiblePages(), canSettings, enabled, modPages };
  const route = resolveRoute(path, ctx);
  const nav = buildNav(ctx);

  const [open, setOpen] = useState(() => expandFor(loadOpen(), ancestorsOf(route)));
  // Navigating (or going back) opens the groups that hold the page.
  const trail = ancestorsOf(route).join(" ");
  useEffect(() => {
    setOpen((o) => {
      const next = expandFor(o, trail ? trail.split(" ") : []);
      if (next !== o) saveOpen(next);
      return next;
    });
  }, [trail]);
  const toggle = useCallback((id: string) => {
    setOpen((o) => {
      const next = { ...o, [id]: !o[id] };
      saveOpen(next);
      return next;
    });
  }, []);

  useEffect(() => {
    const onPop = () => setPath(sync());
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);
  const go = (href: string) => {
    history.pushState(null, "", href);
    setPath(location.pathname);
  };
  const frame = route.kind === "modpage" ? modPages?.find((p) => p.module === route.module && p.path === route.path) : undefined;
  const Current = route.kind === "page" ? pages[route.page as Page] : undefined;
  return (
    <GoContext.Provider value={go}>
    <div className="shell">
      <nav className="nav">
        <div className="wordmark">
          <Mark /> db-branch
        </div>
        <SideNav items={nav} active={activeId(route)} open={open} onToggle={toggle} onGo={go} />
        <span className="spacer" />
        <AccountMenu session={session} where="admin" onSignOut={onLogout} />
        <span className="nav-foot" title={t("db-branch is open source under the Apache License 2.0")}>
          © 2026 DB Branch
        </span>
      </nav>
      <main className="main">
        {frame ? (
          <iframe className="module-frame" title={frame.title} {...moduleFrame(frame.module, frame.path, language())} />
        ) : route.kind === "overview" ? (
          <Modules onOpen={(n) => go(`${ADMIN}/modules/${encodeURIComponent(n)}`)} />
        ) : route.kind === "module" ? (
          <Modules key={route.name} name={route.name} />
        ) : route.kind === "base" ? (
          <BaseDetailPage key={route.name} name={route.name} />
        ) : route.kind === "branch" ? (
          <BranchDetail key={route.name} name={route.name} />
        ) : (
          Current && <Current />
        )}
      </main>
    </div>
    </GoContext.Provider>
  );
}
