import { useEffect, useState } from "react";
import { api } from "../api";
import { ClientId, clientSpecsFor, connectionForProxy, generateClientOutput } from "../lib/clientfiles";
import { Connection, connectionFrom, hostPort, proxyWays, snippets } from "../lib/connect";
import { t } from "../lib/i18n";
import { CopyButton } from "./marks";
import { ErrorLine } from "./ui";

type Tab = "url" | "command" | "env" | "values" | "proxy" | "files";
type Proxy = { host: string; port: number; user: string };

const tabs: [Tab, string][] = [
  ["url", "URL"],
  ["command", "Command"],
  ["env", ".env"],
  ["values", "Values"],
];
const proxyTab: [Tab, string] = ["proxy", "ProxySQL"];
const filesTab: [Tab, string] = ["files", "Client file"];

export function downloadTextFile(filename: string, mime: string, content: string | Uint8Array): void {
  const blob = new Blob([content as BlobPart], { type: mime });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

export function ClientFilePanel({
  branch,
  conn,
  proxy,
  reveal,
  onToggleReveal,
}: {
  branch: string;
  conn: Connection;
  proxy?: Proxy | null;
  reveal: boolean;
  onToggleReveal?: () => void;
}) {
  const clients = clientSpecsFor(conn.engine);
  const [clientId, setClientId] = useState<ClientId>(() => clients[0]?.id ?? "dbeaver");
  const [target, setTarget] = useState<"direct" | "proxy">("direct");
  const [includePw, setIncludePw] = useState(false);

  useEffect(() => {
    if (!clients.some((c) => c.id === clientId) && clients[0]) {
      setClientId(clients[0].id);
    }
  }, [clientId, clients]);

  const selectedSpec = clients.find((c) => c.id === clientId) ?? clients[0];
  if (!selectedSpec) {
    return <p className="muted small">{t("No client configuration file is available for this engine.")}</p>;
  }

  const showProxy = !!proxy && conn.engine !== "postgres";
  const viaProxy = showProxy && target === "proxy";
  const activeConn = viaProxy && proxy ? connectionForProxy(conn, proxy) : conn;
  const hasPlaintext =
    (selectedSpec.supportsFile(activeConn.engine) && selectedSpec.filePasswordMode === "plaintext") ||
    (selectedSpec.supportsClipboard(activeConn.engine) && selectedSpec.clipboardPasswordMode === "plaintext");
  const effectiveIncludePw = hasPlaintext && includePw;

  const out = generateClientOutput(selectedSpec.id, activeConn, branch, { includePassword: effectiveIncludePw, viaProxy });
  const previewOut = generateClientOutput(selectedSpec.id, activeConn, branch, {
    includePassword: effectiveIncludePw,
    maskPassword: effectiveIncludePw && !reveal,
    viaProxy,
  });
  const previewSections = [
    ...(previewOut.file ? [{ title: previewOut.file.filename, text: previewOut.file.content }] : []),
    ...(previewOut.clipboard ? [{ title: "URL", text: previewOut.clipboard.text }] : []),
  ];

  return (
    <div className="connect-files">
      <div className="connect-file-controls">
        <select aria-label={t("Client")} value={selectedSpec.id} onChange={(e) => setClientId(e.target.value as ClientId)}>
          {clients.map((c) => (
            <option key={c.id} value={c.id}>
              {c.label}
            </option>
          ))}
        </select>
        {showProxy && (
          <div className="segmented small" role="group" aria-label={t("Target")}>
            <button type="button" className={!viaProxy ? "on" : ""} aria-pressed={!viaProxy} onClick={() => setTarget("direct")}>
              {t("Direct")}
            </button>
            <button type="button" className={viaProxy ? "on" : ""} aria-pressed={viaProxy} onClick={() => setTarget("proxy")}>
              {t("ProxySQL")}
            </button>
          </div>
        )}
        <div className="connect-file-actions">
          {out.file && (out.file.mime !== "application/zip" || !!out.file.bytes) && (
            <button
              type="button"
              onClick={() => downloadTextFile(out.file!.filename, out.file!.mime, out.file!.bytes ?? out.file!.content)}
            >
              {t("Download")}
            </button>
          )}
          {out.clipboard && (
            <CopyButton
              showState
              text={t(out.clipboard.labelKey)}
              label={t(out.clipboard.labelKey)}
              load={() => Promise.resolve(out.clipboard!.text)}
            />
          )}
        </div>
      </div>
      <label className="connect-check small">
        <input
          type="checkbox"
          checked={effectiveIncludePw}
          disabled={!hasPlaintext}
          onChange={(e) => setIncludePw(e.target.checked)}
        />{" "}
        {t("Include password")}
      </label>
      <p className="muted small">{t(selectedSpec.hintKey)}</p>
      {hasPlaintext ? (
        <>
          {selectedSpec.filePasswordMode === "none" && selectedSpec.clipboardPasswordMode === "plaintext" && (
            <p className="muted small">{t("dataSources.xml omits the password; only the copied JDBC URL includes it when checked.")}</p>
          )}
          <p className={effectiveIncludePw ? "connect-warn small" : "muted small"}>
            {effectiveIncludePw
              ? t("The exported file or copied URL includes the plaintext password: do not share it or post it in chats or tickets.")
              : t("This format stores the password in plaintext when included. By default it is omitted; enter it in the client.")}
          </p>
        </>
      ) : (
        <p className="muted small">{t("This format does not store passwords in plaintext. Enter the password when connecting.")}</p>
      )}
      {previewSections.length > 0 && (
        <details className="connect-preview">
          <summary className="muted small">{t("Preview file")}</summary>
          {effectiveIncludePw && onToggleReveal && (
            <button type="button" className="text-button small" onClick={onToggleReveal} aria-pressed={reveal}>
              {reveal ? t("Hide password") : t("Show password")}
            </button>
          )}
          {previewSections.map((sec) => (
            <div key={sec.title} className="connect-snippet">
              <pre className="mono">{sec.text}</pre>
            </div>
          ))}
        </details>
      )}
    </div>
  );
}

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
  const snip = (tab === "proxy" || tab === "values" || tab === "files" ? "url" : tab) as "url" | "command" | "env";
  const showProxy = !!proxy && conn.engine !== "postgres";
  const px = showProxy ? proxyWays(proxy, branch, conn.engine) : null;
  const visibleTabs = showProxy ? [...tabs, proxyTab, filesTab] : [...tabs, filesTab];

  return (
    <div className="connect">
      <div className="connect-bar">
        <div className="segmented small" role="tablist" aria-label={t("Connection format")}>
          {visibleTabs.map(([id, l]) => (
            <button key={id} type="button" role="tab" aria-selected={tab === id} className={tab === id ? "on" : ""} onClick={() => setTab(id)}>
              {t(l)}
            </button>
          ))}
        </div>
      {tab !== "files" && (
        <button type="button" className="text-button small" onClick={() => setReveal(!reveal)} aria-pressed={reveal}>
          {reveal ? t("Hide password") : t("Show password")}
        </button>
      )}
      </div>
      {tab === "files" ? (
        <ClientFilePanel key={`${branch}:${conn.engine}`} branch={branch} conn={conn} proxy={proxy} reveal={reveal} onToggleReveal={() => setReveal(!reveal)} />
      ) : tab === "proxy" && px ? (
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
      {tab !== "files" && (
        <p className="muted small">
          {tab === "url" && t("Paste into a GUI tool (TablePlus, DBeaver, DataGrip: “from URL”) or anything that takes a database URL.")}
          {tab === "command" &&
            (conn.engine === "postgres" ? t("Run in a terminal with psql installed.") : t("Run in a terminal with the mysql client; it asks for the password (Values tab)."))}
          {tab === "env" && t("For an app's .env: frameworks read DATABASE_URL; the DB_* lines suit the rest.")}
          {tab === "values" && t("For tools that ask for each field separately.")}
          {tab === "proxy" && t("Through ProxySQL, sign in with the user name above, or add the query comment to the first statement of a shared connection.")}{" "}
          {t("Copies include the password: do not paste them into chats or tickets.")}
        </p>
      )}
    </div>
  );
}
