import { useEffect, useState } from "react";
import { api } from "../api";
import { Connection, connectionFrom, hostPort, proxyWays, snippets } from "../lib/connect";
import { t } from "../lib/i18n";
import { CopyButton } from "./marks";
import { ErrorLine } from "./ui";

type Tab = "url" | "command" | "env" | "values" | "proxy";
type Proxy = { host: string; port: number; user: string };

const tabs: [Tab, string][] = [
  ["url", "URL"],
  ["command", "Command"],
  ["env", ".env"],
  ["values", "Values"],
];
const proxyTab: [Tab, string] = ["proxy", "ProxySQL"];

// ConnectPanel shows how to reach a branch in the form people need, each
// with a copy button. The password stays masked until "Show"; copies
// always carry the real one.
export function ConnectPanel({ branch, proxy }: { branch: string; proxy?: Proxy | null }) {
  const [conn, setConn] = useState<Connection | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [tab, setTab] = useState<Tab>("url");
  const [reveal, setReveal] = useState(false);
  useEffect(() => {
    api.dsn(branch).then(
      (d) => setConn(connectionFrom(d)),
      (e: unknown) => setError(e),
    );
  }, [branch]);
  if (error) return <ErrorLine error={error} />;
  if (!conn) return <p className="muted small">{t("Loading…")}</p>;
  const s = snippets(conn, reveal);
  const copy = (v: string) => () => Promise.resolve(v);
  const snip = (tab === "proxy" || tab === "values" ? "url" : tab) as "url" | "command" | "env";
  const showProxy = !!proxy && conn.engine !== "postgres";
  const px = showProxy ? proxyWays(proxy, branch, conn.engine) : null;

  return (
    <div className="connect">
      <div className="connect-bar">
        <div className="segmented small" role="tablist" aria-label={t("Connection format")}>
          {(showProxy ? [...tabs, proxyTab] : tabs).map(([id, l]) => (
            <button key={id} type="button" role="tab" aria-selected={tab === id} className={tab === id ? "on" : ""} onClick={() => setTab(id)}>
              {t(l)}
            </button>
          ))}
        </div>
        <button type="button" className="text-button small" onClick={() => setReveal(!reveal)} aria-pressed={reveal}>
          {reveal ? t("Hide password") : t("Show password")}
        </button>
      </div>
      {tab === "proxy" && px ? (
        <dl className="connect-values">
          {(
            [
              ["Host:port", px.hostPort],
              ["User", px.user],
              ["Command", px.command],
              ["Query comment", px.comment],
            ] as [string, string][]
          ).map(([k, v]) => (
            <div key={k} className="connect-row">
              <dt>{t(k)}</dt>
              <dd className="mono" title={v}>{v}</dd>
              <CopyButton showState label={t("Copy {what}", { what: t(k) })} load={copy(v)} />
            </div>
          ))}
        </dl>
      ) : tab === "values" ? (
        <dl className="connect-values">
          {(
            [
              ["Host", conn.host],
              ["Port", String(conn.port)],
              ["Host:port", hostPort(conn)],
              ["User", conn.user],
              ["Password", conn.password],
              ...(conn.database ? [["Database", conn.database]] : []),
            ] as [string, string][]
          ).map(([k, v]) => (
            <div key={k} className="connect-row">
              <dt>{t(k)}</dt>
              <dd className="mono">{k === "Password" && !reveal ? "••••••" : v}</dd>
              <CopyButton showState label={t("Copy {what}", { what: t(k) })} load={copy(v)} />
            </div>
          ))}
        </dl>
      ) : (
        <div className="connect-snippet">
          <pre className="mono">{s[snip].shown}</pre>
          <CopyButton showState label={t("Copy")} load={copy(s[snip].copy)} />
        </div>
      )}
      <p className="muted small">
        {tab === "url" && t("Paste into a GUI tool (TablePlus, DBeaver, DataGrip: “from URL”) or anything that takes a database URL.")}
        {tab === "command" &&
          (conn.engine === "postgres" ? t("Run in a terminal with psql installed.") : t("Run in a terminal with the mysql client; it asks for the password (Values tab)."))}
        {tab === "env" && t("For an app's .env: frameworks read DATABASE_URL; the DB_* lines suit the rest.")}
        {tab === "values" && t("For tools that ask for each field separately.")}
        {tab === "proxy" && t("Through ProxySQL, sign in with the user name above, or add the query comment to the first statement of a shared connection.")}{" "}
        {t("Copies include the password: do not paste them into chats or tickets.")}
      </p>
    </div>
  );
}
