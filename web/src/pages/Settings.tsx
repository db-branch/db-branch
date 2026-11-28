import { useEffect, useState } from "react";
import { api, BackupFile, can, isAdmin, UpdateView } from "../api";
import { bytes, formatDateTime } from "../lib/format";
import { ScopeForm } from "../components/scopeform";
import { TokenManager } from "../components/tokens";
import { ErrorLine, ErrorText } from "../components/ui";
import { serverError } from "../lib/errors";
import { useLive } from "../lib/live";
import { t } from "../lib/i18n";

// Settings are split into tabs: General (db-branch's own), Service tokens
// and Updates. The tab is in the URL: /admin/settings/<tab>. Modules have
// their own place in the sidebar: /admin/modules (see pages/Modules.tsx).
const BASE = "/admin/settings";

function currentTab(): string {
  return location.pathname.startsWith(BASE + "/") ? decodeURIComponent(location.pathname.slice(BASE.length + 1).split("/")[0]) : "general";
}

export function Settings() {
  const { data: scopes, error } = useLive(api.settings, ["setting", "module"]);
  const [tab, setTab] = useState(currentTab());
  useEffect(() => {
    const onPop = () => setTab(currentTab());
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);
  const go = (t: string) => {
    history.pushState(null, "", t === "general" ? BASE : `${BASE}/${encodeURIComponent(t)}`);
    setTab(t);
  };

  // The licence key is set in the License card on the Modules page, with the
  // modules it unlocks, not among the core settings.
  const coreAll = scopes?.find((s) => s.scope === "core");
  const core = coreAll && { ...coreAll, settings: coreAll.settings.filter((x) => x.key !== "license.key") };
  const canUpdate = isAdmin() || can("update.read");
  const tabs: [string, string][] = [["general", "General"], ["tokens", "Service tokens"], ...(canUpdate ? [["updates", "Updates"] as [string, string]] : [])];
  const shown = tabs.some(([t]) => t === tab) ? tab : "general";

  return (
    <section className="page">
      <header className="page-head">
        <h1>{t("Settings")}</h1>
      </header>
      <nav className="tabs" role="tablist" aria-label={t("Settings sections")}>
        {tabs.map(([id, label]) => (
          <a
            key={id}
            role="tab"
            aria-selected={shown === id}
            className={shown === id ? "on" : ""}
            href={id === "general" ? BASE : `${BASE}/${id}`}
            onClick={(e) => {
              e.preventDefault();
              go(id);
            }}
          >
            {t(label)}
          </a>
        ))}
      </nav>
      <ErrorLine error={error} />
      {shown === "general" && core && <ScopeForm scope={core} />}
      {shown === "general" && <Backups />}
      {shown === "tokens" && <Tokens />}
      {shown === "updates" && <Updates />}
    </section>
  );
}

// Updates: installed versions per node, a check of the package repository,
// the newest applicable version (minor, or 0.x -> 1.x) with a confirmation
// step, and the live progress of the rollout. Other major versions are only
// shown: they are updated by hand.
function Updates() {
  const { data, error, reload } = useLive(api.updates, ["update"]);
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState<string | null>(null);
  const [cancelling, setCancelling] = useState(false);
  const [actionError, setActionError] = useState<unknown>(null);
  const act = (p: Promise<unknown>, done?: () => void) => {
    setBusy(true);
    p.then(
      () => (setActionError(null), setBusy(false), done?.(), reload()),
      (e: unknown) => (setActionError(e), setBusy(false)),
    );
  };
  return (
    <UpdatesPanel
      v={data}
      error={error ?? actionError}
      busy={busy}
      confirming={confirming}
      cancelling={cancelling}
      onCheck={() => act(api.checkUpdates())}
      onConfirmVersion={setConfirming}
      onCancelConfirm={() => setConfirming(null)}
      onStartUpdate={(version) => act(api.startUpdate(version), () => setConfirming(null))}
      onCancelRollout={() => (cancelling ? (setCancelling(false), act(api.cancelUpdate())) : setCancelling(true))}
    />
  );
}

export function UpdateConfirm({ version, major0, busy, onCancel, onConfirm }: { version: string; major0: boolean; busy: boolean; onCancel: () => void; onConfirm: () => void }) {
  return (
    <div className="confirm-box" role="group" aria-label={t("Confirm the update")}>
      {major0 && (
        <p className="warn">
          {t("Major update (0.x → 1.0). Read the upgrade notes in the release notes before starting.")}
        </p>
      )}
      <p>
        {t("Update every node to {version}? One node at a time restarts its agent (branches and bases stop and start again as on any service restart). It stops if a node fails; there is no automatic rollback.", { version })}
      </p>
      <div className="form-actions">
        <button onClick={onCancel} disabled={busy}>
          {t("Cancel")}
        </button>
        <button className="danger-button" disabled={busy} onClick={onConfirm}>
          {busy ? t("Starting…") : t("Start the update")}
        </button>
      </div>
    </div>
  );
}

export function UpdatesPanel({
  v,
  error,
  busy,
  confirming,
  cancelling,
  onCheck,
  onConfirmVersion,
  onCancelConfirm,
  onStartUpdate,
  onCancelRollout,
}: {
  v: UpdateView | undefined;
  error: unknown;
  busy: boolean;
  confirming: string | null;
  cancelling: boolean;
  onCheck: () => void;
  onConfirmVersion: (version: string) => void;
  onCancelConfirm: () => void;
  onStartUpdate: (version: string) => void;
  onCancelRollout: () => void;
}) {
  const r = v?.rollout;
  const running = r?.status === "running";
  const core = v?.packages.find((p) => p.name === "db-branch");
  const majors = (core?.available ?? []).filter((c) => c.kind === "major");
  const confirmingMajor0 = Boolean(confirming && core?.available.some((c) => c.version === confirming && c.kind === "major0"));
  const stateLabel: Record<string, string> = { pending: "Waiting", updating: "Updating…", done: "Updated", failed: "Failed" };
  return (
    <section className="settings-card">
      <h2 className="section-title">{t("Updates")}</h2>
      <p className="field-hint">
        {t("Update the cluster to a newer minor or patch version (or 1.x from 0.x) without logging in to the nodes. Followers update first, one at a time; the leader goes last. Each node restarts its agent.")}
      </p>
      <ErrorLine error={error} />
      {v && (
        <table className="rows compact">
          <tbody>
            {v.nodes.map((n) => (
              <tr key={n.name}>
                <td className="mono">
                  {n.name}
                  {n.leader && <span className="badge">{t("leader")}</span>}
                </td>
                <td className="mono">{n.version}</td>
                <td>
                  {n.down ? (
                    <span className="state-badge bad">{t("down")}</span>
                  ) : n.update && r ? (
                    <span className={`state-badge ${n.update.state === "done" ? "ok" : n.update.state === "failed" ? "bad" : ""}`}>{t(stateLabel[n.update.state] ?? n.update.state)}</span>
                  ) : (
                    <span className="muted">{t("up")}</span>
                  )}
                  {n.update?.error && <span className="field-hint"><ErrorText error={serverError(n.update.error)} /></span>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <div className="backup-bar">
        <button onClick={onCheck} disabled={busy || running}>
          {busy && !confirming ? t("Checking…") : t("Check for updates")}
        </button>
        <span className="muted small">
          {v?.checked_at && !v.checked_at.startsWith("0001") ? t("Checked {when} by {node}", { when: formatDateTime(v.checked_at), node: v.checked_by }) : t("Not checked yet. Checking contacts the package repository.")}
        </span>
      </div>
      {v && v.packages.some((p) => p.available.length > 0) && (
        <table className="rows compact">
          <tbody>
            {v.packages.map((p) => (
              <tr key={p.name}>
                <td className="mono">{p.name}</td>
                <td className="mono">
                  {p.installed}
                  {p.mixed && <span className="badge">{t("differs between nodes")}</span>}
                </td>
                <td>
                  {p.available.length === 0 && <span className="muted">{t("up to date")}</span>}
                  {p.available.map((c) => (
                    <span key={c.version} className="mono update-candidate">
                      {c.version}{" "}
                      {c.kind === "major" ? (
                        <span className="muted">{t("major update — see docs")}</span>
                      ) : c.kind === "major0" ? (
                        <span className="muted">{t("major update — 0.x → 1.x")}</span>
                      ) : (
                        <span className="muted">{c.kind === "minor" ? t("minor") : c.kind}</span>
                      )}{" "}
                    </span>
                  ))}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {v && v.latest_minor && !running && !confirming && (
        <div className="form-actions">
          <button className="primary" onClick={() => onConfirmVersion(v.latest_minor)} disabled={busy}>
            {t("Update to {version}", { version: v.latest_minor })}
          </button>
        </div>
      )}
      {confirming && !running && (
        <UpdateConfirm version={confirming} major0={confirmingMajor0} busy={busy} onCancel={onCancelConfirm} onConfirm={() => onStartUpdate(confirming)} />
      )}
      {majors.length > 0 && !v?.latest_minor && <p className="muted small">{t("A major update is done by hand: see docs/updates.md.")}</p>}
      {r && (
        <div className="rollout">
          <h3>{t("Update to {version}", { version: r.target })}</h3>
          <p className={r.status === "failed" ? "error-line" : r.status === "done" ? "ok-line" : "muted"} role={r.status === "failed" ? "alert" : undefined}>
            {r.status === "running" && t("In progress.")}
            {r.status === "done" && t("Finished.")}
            {r.status === "canceled" && t("Canceled.")}
            {r.status === "failed" && (
              <>
                {t("Stopped.")} {r.error && <ErrorText error={serverError(r.error)} />}
              </>
            )}
          </p>
          <ol className="rollout-steps">
            {r.order.map((n) => (
              <li key={n}>
                <span className="mono">{n}</span> <span className="muted">{t(stateLabel[r.nodes[n]?.state] ?? "")}</span>
              </li>
            ))}
          </ol>
          {(r.notes ?? []).map((n) => (
            <p key={n} className="field-hint">
              {n}
            </p>
          ))}
          {running && (
            <div className="form-actions">
              <button className="danger-button" disabled={busy} onClick={onCancelRollout}>
                {cancelling ? t("Really stop? Nodes that have not started will not.") : t("Stop the rollout")}
              </button>
            </div>
          )}
          {r.status === "failed" && <p className="field-hint">{t("Fix the cause on the node, then check for updates and start again. Nodes already updated are skipped.")}</p>}
        </div>
      )}
    </section>
  );
}

// Tokens: API tokens for scripts and LLM agents (MCP). A new token is shown
// once, with the command that connects an MCP client.
function Tokens() {
  return (
    <TokenManager
      title={t("Service tokens")}
      hint={
        <>
          {t("For CI and integrations: they act as themselves, not as a person, and an admin manages them here. People create their own MCP tokens on their")}{" "}
          <a href="/account">{t("Account")}</a> {t("page; those act as them.")}
        </>
      }
      list={api.tokens}
      create={(t) => api.createToken({ name: t.name, scope: t.scope ?? "branches", ttl: t.ttl })}
      revoke={api.revokeToken}
      withScope
      placeholder={t("name, e.g. ci-deploy")}
    />
  );
}

// Backups lists each node's saved cluster states (settings, users,
// tokens, module data), takes one now, and downloads one for off-site
// keeping. Restoring is done on a node: db-branch backup restore <file>.
function Backups() {
  const { data: cluster } = useLive(api.cluster, ["node"]);
  const nodes = cluster?.nodes.map((n) => n.name) ?? [];
  const [node, setNode] = useState("");
  const target = node || cluster?.leader || nodes[0] || "";
  const { data, error, reload } = useLive(() => api.backups(target || undefined), ["setting"]);
  // Load again when the node is known or chosen: links point at the node
  // that listed the files.
  useEffect(() => reload(), [target, reload]);
  const listed = data?.node ?? target;
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<unknown>(null);
  const now = () => {
    setBusy(true);
    api.takeBackup(target || undefined).then(
      () => (setActionError(null), setBusy(false), reload()),
      (e: unknown) => (setActionError(e), setBusy(false)),
    );
  };
  const list: BackupFile[] = data?.items ?? [];
  return (
    <section className="settings-card">
      <h2 className="section-title">{t("Backups")}</h2>
      <p className="field-hint">
        {t("Every node saves the cluster state (settings, users, tokens, module data) at the interval above. The files hold secrets: keep downloads safe.")}{" "}
        {t("Restore on a node:")} <code className="mono">db-branch backup restore &lt;file&gt;</code>{" "}
        {t("Base data is not included yet.")}
      </p>
      <div className="backup-bar">
        {nodes.length > 1 && (
          <select className="mono" value={target} onChange={(e) => setNode(e.target.value)} aria-label={t("Node")}>
            {nodes.map((n) => (
              <option key={n}>{n}</option>
            ))}
          </select>
        )}
        <button onClick={now} disabled={busy}>
          {busy ? t("Saving…") : t("Back up now")}
        </button>
      </div>
      <ErrorLine error={error ?? actionError} />
      {list.length === 0 && !error && <p className="muted small">{t("No backups on this node yet.")}</p>}
      {list.length > 0 && (
        <div className="table-wrap">
          <table className="rows compact">
            <tbody>
              {list.map((f) => (
                <tr key={f.name}>
                  <td className="mono">{f.name}</td>
                  <td className="muted">{bytes(f.size)}</td>
                  <td className="muted">{formatDateTime(f.taken_at)}</td>
                  <td className="col-actions">
                    <a href={`/v1/backups/${encodeURIComponent(f.name)}${listed ? `?node=${encodeURIComponent(listed)}` : ""}`} download className="link-action">
                      {t("Download")}
                    </a>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
