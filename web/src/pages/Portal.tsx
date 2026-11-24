// Portal is the page for people who use branches (not run the cluster):
// pick a base, get a branch and its connection, manage your own branches.
// It works without logging in for public snapshots.
import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError, Branch, onUnauthorized, PublicBase, Session, ssoReturn } from "../api";
import { Login, Mark } from "../App";
import { AccountMenu, AccountPage, LanguagePicker } from "../components/account";
import { allowed, BranchRow, Job, SnapInfo } from "../components/branchcard";
import { BranchHistory } from "../components/branchhistory";
import { FirstPassword } from "../components/firstpassword";
import { SqlRunner } from "../components/sqlrunner";
import { engineLabel } from "../lib/format";
import { hasHistoryModule } from "../lib/history";
import { Empty, ErrorLine } from "../components/ui";
import { Limits, NewBranch, NewBranchSkeleton } from "../components/newbranch";
import { Choice } from "../lib/bases";
import { chooseLanguage, remembered as rememberedLanguage, setLanguage, t } from "../lib/i18n";
import { useCapabilities } from "../lib/caps";
import { disconnect, useNow } from "../lib/live";
import { sortBranches } from "../lib/branches";
import { seenTour, showsTour, tourSteps } from "../lib/tour";
import { Tour } from "../components/tour";
import { Icon } from "../components/marks";

// Branches made here without logging in are remembered in this browser.
const MINE = "db-branch.my-branches";
function remembered(): string[] {
  try {
    return JSON.parse(localStorage.getItem(MINE) ?? "[]");
  } catch {
    return [];
  }
}
function remember(names: string[]) {
  try {
    localStorage.setItem(MINE, JSON.stringify(names));
  } catch {
    /* storage unavailable: the list lasts this visit */
  }
}

// usePoll reloads every few seconds (events need a login; polling works for everyone).
// `scope` names what the data belongs to (the signed-in person): when it changes the old data is dropped, never shown for the new one.
function usePoll<T>(load: () => Promise<T>, deps: unknown[], every = 4000, scope?: unknown, enabled = true) {
  const [data, setData] = useState<T>();
  const [seen, setSeen] = useState(scope);
  if (seen !== scope) {
    setSeen(scope);
    setData(undefined);
  }
  const [error, setError] = useState<string | null>(null);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const run = useCallback(() => load().then((d) => (setData(d), setError(null)), (e: Error) => setError(e.message)), deps);
  useEffect(() => {
    if (!enabled) return;
    run();
    const t = setInterval(run, every);
    return () => clearInterval(t);
  }, [run, every, enabled]);
  return { data, error, reload: run };
}

export function nextPortalSessionAction(s: Session | null): "anonymous" | "first_password" | "in" {
  if (!s) return "anonymous";
  return s.must_change_password ? "first_password" : "in";
}

type Snaps = Record<string, Record<string, SnapInfo>>;
type Offer = { choices: Choice[]; snaps: Snaps; limits?: Limits; perBase?: Record<string, Limits> };

export function Portal() {
  const [session, setSession] = useState<Session | null | undefined>(undefined);
  const [mustChange, setMustChange] = useState(false);
  const [signingIn, setSigningIn] = useState(false);
  const [ssoError, setSsoError] = useState<string | undefined>();
  const [local, setLocal] = useState<string[]>(remembered());
  const [fresh, setFresh] = useState<string | null>(null);
  const [touring, setTouring] = useState(false);
  const [jobs, setJobs] = useState<Job[]>([]);
  // The branch's own page is /branch/<name>; anything else is the list.
  const [path, setPath] = useState(location.pathname);
  const pathRef = useRef(path);
  pathRef.current = path;
  useEffect(() => {
    const onPop = () => setPath(location.pathname);
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);
  const go = useCallback((href: string) => {
    history.pushState(null, "", href);
    setPath(location.pathname);
    window.scrollTo(0, 0);
  }, []);
  const branchName = path.startsWith("/branch/") ? decodeURIComponent(path.slice("/branch/".length).split("/")[0]) : null;
  const pageOf = (name: string) => `/branch/${encodeURIComponent(name)}`;
  const [announce, setAnnounce] = useState("");
  const now = useNow(15000);
  const alive = useRef(true);
  useEffect(() => () => void (alive.current = false), []);
  const enterSession = useCallback((s: Session) => {
    setLanguage(chooseLanguage(s.language || rememberedLanguage(), navigator.language));
    setMustChange(false);
    setSession(s);
  }, []);
  useEffect(() => {
    ssoReturn().then(({ session: s, error }) => {
      if (s) {
        const action = nextPortalSessionAction(s);
        if (action === "first_password") {
          setLanguage(chooseLanguage(s.language || rememberedLanguage(), navigator.language));
          setMustChange(true);
          return;
        }
        return enterSession(s);
      }
      if (error) {
        setSsoError(error);
        setSigningIn(true);
      }
      api.session().then(
        (sess) => {
          const action = nextPortalSessionAction(sess);
          if (action === "first_password") {
            setLanguage(chooseLanguage(sess.language || rememberedLanguage(), navigator.language));
            setMustChange(true);
          } else {
            enterSession(sess);
          }
        },
        () => setSession(null),
      );
    });
  }, [enterSession]);
  useEffect(() => {
    if (!session && !mustChange) return;
    return onUnauthorized(() => {
      disconnect();
      setMustChange(false);
      setSession(null);
      setSigningIn(true);
    });
  }, [session, mustChange]);
  const myId = session ? session.user.id : null;
  const myName = session ? session.user.name || session.user.id : null;

  const pollReady = session !== undefined && !mustChange;
  const caps = useCapabilities(!!session, pollReady);
  const bases = usePoll<Offer>(
    async () => {
      if (session) {
        const all = await api.bases();
        const snaps: Snaps = {};
        for (const b of all) snaps[b.name] = Object.fromEntries(b.snapshots.map((x) => [x.name, { created_at: x.created_at, masked: !!x.mask }]));
        return {
          snaps,
          choices: all
            .filter((b) => b.status === "ready" && b.latest_snapshot)
            .map((b) => {
              const latest = snaps[b.name][b.latest_snapshot!];
              return { name: b.name, runtime: b.runtime, description: b.description, snapshot: b.latest_snapshot!, snapshotAt: latest?.created_at ?? null, public: b.snapshots.some((s) => s.public), masked: !!latest?.masked };
            }),
        };
      }
      const pub = await api.publicBases();
      const snaps: Snaps = {};
      for (const b of pub.items) snaps[b.name] = Object.fromEntries(b.snapshots.map((x) => [x.name, { created_at: x.created_at, masked: false }]));
      return {
        snaps,
        choices: pub.items.map((b: PublicBase) => {
          const last = b.snapshots[b.snapshots.length - 1];
          return { name: b.name, runtime: b.runtime, description: b.description, snapshot: last.name, snapshotAt: last.created_at, public: true, masked: false };
        }),
        limits: pub.anonymous,
        // What each base allows (the stricter of its own and the cluster-wide limits).
        perBase: Object.fromEntries(pub.items.filter((b) => b.anonymous).map((b) => [b.name, b.anonymous!])),
      };
    },
    [session],
    15000,
    session,
    pollReady,
  );

  const mine = usePoll<Branch[]>(
    async () => {
      if (session) return (await api.branches()).filter((b) => !!myId && b.owner === myId);
      const found: Branch[] = [];
      for (const n of local) {
        try {
          found.push(await api.branch(n));
        } catch (e) {
          if (e instanceof ApiError && e.status === 404) continue; // expired or deleted
          throw e;
        }
      }
      if (found.length !== local.length) {
        const keep = found.map((b) => b.name);
        remember(keep);
        setLocal(keep);
      }
      return found;
    },
    [session, local.join(",")],
    jobs.some((j) => j.state === "running") ? 1500 : 4000,
    session,
    pollReady,
  );

  // track follows a branch being made: it shows at once as "provisioning",
  // opens its connection when ready, and says in words if it failed.
  const track = async (job: Job, operation: string) => {
    if (!session) {
      const next = [job.name, ...local.filter((n) => n !== job.name)];
      remember(next);
      setLocal(next);
    }
    setJobs((js) => [job, ...js.filter((j) => j.name !== job.name)]);
    setFresh(job.name);
    setAnnounce(t("Creating {name}…", { name: job.name }));
    mine.reload();
    try {
      for (;;) {
        const o = await api.operation(operation);
        if (o.state === "failed") throw new Error(o.error ?? "creation failed");
        if (o.state === "succeeded") break;
        await new Promise((r) => setTimeout(r, 1000));
        if (!alive.current) return;
      }
      await mine.reload();
      if (!alive.current) return;
      setJobs((js) => js.filter((j) => j.name !== job.name));
      // Its connection is on its page: go there unless the person went elsewhere meanwhile.
      if (pathRef.current === "/") go(pageOf(job.name));
      setAnnounce(t("{name} is ready. Its connection is open.", { name: job.name }));
    } catch (err) {
      if (!alive.current) return;
      const error = (err as Error).message;
      setJobs((js) => js.map((j) => (j.name === job.name ? { ...j, state: "failed", error } : j)));
      setAnnounce(t("Could not create {name}: {error}", { name: job.name, error }));
    }
  };
  const newBranchButton = (
    <button
      type="button"
      className="primary to-create"
      onClick={() => {
        document.getElementById("new-branch")?.scrollIntoView({ behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth", block: "start" });
        document.querySelector<HTMLInputElement>(".base-search")?.focus({ preventScroll: true });
      }}
    >
      <Icon name="plus" /> {t("New branch")}
    </button>
  );
  const rows = sortBranches(
    [
      ...(mine.data ?? []),
      ...jobs.filter((j) => !mine.data?.some((b) => b.name === j.name)).map((j) => placeholder(j, myId, myName)),
    ],
    [...jobs.map((j) => j.name), ...(fresh ? [fresh] : [])],
  );

  // First visit: open the tour once the form and the list are on the page.
  const userId = session?.user.id;
  const ready = !!bases.data && bases.data.choices.length > 0 && !!mine.data;
  useEffect(() => {
    if (ready && userId && showsTour(session) && location.pathname !== "/account" && !seenTour.has(userId)) {
      seenTour.mark(userId);
      setTouring(true);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ready, userId]);
  const canTour = ready && showsTour(session) && location.pathname !== "/account";

  if (mustChange)
    return (
      <main className="login">
        <div className="wordmark">
          <Mark /> db-branch
        </div>
        <FirstPassword onDone={enterSession} onSignOut={() => api.signOut(() => { setMustChange(false); setSession(null); })} />
      </main>
    );
  if (session === undefined) return null;
  if (signingIn)
    return (
      <Login
        onDone={(s) => {
          setLanguage(chooseLanguage(s.language || rememberedLanguage(), navigator.language));
          setSession(s);
          setSigningIn(false);
        }}
        onCancel={() => setSigningIn(false)}
        initialError={ssoError}
      />
    );

  return (
    <div className="portal">
      <header className="portal-head">
        <a className="wordmark" href="/">
          <Mark /> db-branch
        </a>
        <span className="spacer" />
        {canTour && (
          <button type="button" className="text-button" onClick={() => setTouring(true)}>
            {t("Tour")}
          </button>
        )}
        {session ? (
          <AccountMenu session={session} where="user" onSignOut={() => (location.pathname === "/account" ? location.assign("/") : setSession(null))} />
        ) : (
          <>
            <LanguagePicker />
            <button onClick={() => setSigningIn(true)}>{t("Sign in")}</button>
          </>
        )}
      </header>
      {location.pathname === "/account" ? (
        <main className="portal-main">
          {session ? (
            <AccountPage session={session} />
          ) : (
            <p className="muted">
              <button onClick={() => setSigningIn(true)}>{t("Sign in")}</button> {t("to see your account.")}
            </p>
          )}
        </main>
      ) : branchName ? (
        <main className="portal-main portal-branch">
          <a
            className="text-button back-link"
            href="/"
            onClick={(e) => {
              e.preventDefault();
              go("/");
            }}
          >
            ← {t("Your branches")}
          </a>
          <ErrorLine error={mine.error} />
          {!mine.data && !mine.error && <ul className="brows" aria-hidden="true"><li className="brow skeleton" /></ul>}
          {mine.data && !rows.some((x) => x.name === branchName) && <Empty line={t("No such branch.")} />}
          {rows
            .filter((x) => x.name === branchName)
            .map((b) => {
              const job = jobs.find((j) => j.name === b.name);
              return (
                <ul className="brows" key={b.name}>
                  <BranchRow
                    branch={b}
                    job={job}
                    now={now}
                    manage={!!session}
                    can={session?.can}
                    snapshot={bases.data?.snaps[b.base]?.[b.snapshot]}
                    fresh={b.name === fresh}
                    mode="page"
                    sql={session && b.status === "ready" && allowed(session.can, "branch.query") ? <SqlRunner name={b.name} engine={engineLabel(b.runtime).engine} /> : undefined}
                    onChange={mine.reload}
                    onDismiss={job ? () => setJobs((js) => js.filter((j) => j.name !== b.name)) : undefined}
                    prefix={session?.branch_prefix}
                    onOpenParent={(parent) => go(pageOf(parent))}
                    onForked={(job, opId) => {
                      track(job, opId);
                      go(pageOf(job.name));
                    }}
                    extra={session && b.record && hasHistoryModule(caps) && allowed(session.can, "ext.history.read") ? <BranchHistory name={b.name} /> : undefined}
                  />
                </ul>
              );
            })}
        </main>
      ) : (
      <main className={`portal-main portal-two ${rows.length > 0 ? "has-mine" : ""}`}>
        <section className="portal-get">
          <h1>{t("Get a database")}</h1>
          <p className="muted">{t("A full copy of a base, ready in seconds. Change anything; it never touches the original.")}</p>
          <ErrorLine error={bases.error} />
          {!bases.data && !bases.error && <NewBranchSkeleton />}
          {bases.data && bases.data.choices.length === 0 && (
            <Empty line={session ? t("No Base is available to you yet. Ask an administrator to create a Base, take a Snapshot of it and give you access.") : t("No Snapshot is open to everyone yet. Sign in to see the Bases shared with you, or ask an administrator to publish a Snapshot.")} />
          )}
          {bases.data && bases.data.choices.length > 0 && (
            <NewBranch
              key={session ? "signed-in" : "anonymous"}
              choices={bases.data.choices}
              signedIn={!!session}
              prefix={session?.branch_prefix}
              limits={bases.data.limits}
              perBase={bases.data.perBase}
              canRecord={!!caps?.record}
              onStarted={track}
            />
          )}
        </section>
        <section className="portal-mine" data-tour="mine">
          <div className="mine-head">
            <h2>
              {session ? t("Your branches") : t("Branches made in this browser")}
              {rows.length > 0 && <span className="count">{rows.length}</span>}
            </h2>
            {bases.data && bases.data.choices.length > 0 && newBranchButton}
          </div>
          <ErrorLine error={mine.error} />
          <div className="sr-only" role="status" aria-live="polite">
            {announce}
          </div>
          {!mine.data && !mine.error && (
            <ul className="brows" aria-hidden="true">
              {[0, 1, 2].map((i) => (
                <li key={i} className="brow skeleton" />
              ))}
            </ul>
          )}
          {mine.data && rows.length === 0 && (
            <Empty
              line={bases.data && bases.data.choices.length === 0 ? t("No branches yet.") : t("No branches yet. Choose a Base, then press Create branch to get your first one.")}
              action={bases.data && bases.data.choices.length > 0 ? newBranchButton : undefined}
            />
          )}
          {rows.length > 0 && (
            <ul className="brows">
              {rows.map((b, i) => {
                const job = jobs.find((j) => j.name === b.name);
                return (
                  <BranchRow
                    key={b.name}
                    branch={b}
                    job={job}
                    now={now}
                    manage={!!session}
                    can={session?.can}
                    snapshot={bases.data?.snaps[b.base]?.[b.snapshot]}
                    fresh={b.name === fresh}
                    mode="list"
                    href={pageOf(b.name)}
                    onOpen={() => go(pageOf(b.name))}
                    onChange={mine.reload}
                    onDismiss={job ? () => setJobs((js) => js.filter((j) => j.name !== b.name)) : undefined}
                    tourAnchor={i === 0}
                  />
                );
              })}
            </ul>
          )}
        </section>
      </main>
      )}
      {touring && canTour && <Tour steps={tourSteps(session?.branch_prefix)} onClose={() => setTouring(false)} />}
      <footer className="portal-foot muted small">© 2026 DB Branch · {t("db-branch is open source under the Apache License 2.0")}</footer>
    </div>
  );
}

// placeholder stands in for a branch the server has not listed yet.
function placeholder(j: Job, owner: string | null, ownerName: string | null): Branch {
  return {
    name: j.name,
    status: "provisioning",
    status_reason: null,
    base: j.base,
    snapshot: "",
    runtime: j.runtime,
    node: "",
    created_at: new Date(j.startedAt).toISOString(),
    expires_at: null,
    reset_at: null,
    storage: null,
    connection: { host: "", port: 0, user: "" },
    proxy: null,
    operation: null,
    owner,
    owner_name: ownerName,
    id: j.name,
    access: "private",
    checkpoints: false,
  };
}
