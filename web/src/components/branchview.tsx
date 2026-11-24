import type { ReactNode } from "react";
import { api, Branch, ownerLabel } from "../api";
import { extendTtl } from "../lib/branches";
import { engineLabel, formatDateTime, relative } from "../lib/format";
import { serverError } from "../lib/errors";
import { t } from "../lib/i18n";
import { BranchName } from "./branchname";
import { CopyButton, Engine, Status, StorageBar, TtlBar } from "./marks";
import { RecordingBadge } from "./recording";
import { ErrorText } from "./ui";

export type BranchSection = { id: string; title: string; body: ReactNode };

// BranchView is the page of one branch: what it is, the actions on it, and the
// sections the page gives it (connect, checkpoints, upgrade, history). The
// actions are the ones the list used to have in its menu.
export function BranchView({
  b,
  clustered,
  max,
  sections = [],
  allow,
  onBack,
  onExtend,
  onReset,
  onRecreate,
  onDelete,
  onCopyProxy,
  onPause,
  onResume,
  onFork,
  onOpenParent,
}: {
  b: Branch;
  clustered: boolean;
  max: number;
  sections?: BranchSection[];
  allow?: (action: string) => boolean;
  onBack: () => void;
  onExtend: (ttl: string) => void;
  onReset: () => void;
  onRecreate: () => void;
  onDelete: () => void;
  onCopyProxy: () => void;
  onPause: () => void;
  onResume: () => void;
  onFork?: () => void;
  onOpenParent?: (name: string) => void;
}): ReactNode {
  const ready = b.status === "ready";
  const may = (action: string, legacy?: string) => !allow || allow(action) || (legacy ? allow(legacy) : false);
  return (
    <section className="page branch-page">
      <button type="button" className="text-button back-link" onClick={onBack}>
        ← {t("Back to Branches")}
      </button>
      <header className="page-head">
        <span className="page-head-main">
          <h1>
            <BranchName name={b.name} owner={ownerLabel(b)} />
          </h1>
          <Status status={b.status} reason={b.status_reason} />
          {b.access === "open" && (
            <span className="badge" title={t("Anyone may connect, with the shared password")}>
              open
            </span>
          )}
          <RecordingBadge record={b.record} />
        </span>
      </header>
      {b.status_reason && <p className="reason"><ErrorText error={serverError(b.status_reason)} /></p>}
      <dl className="branch-facts">
        <dt>{t("Base")}</dt>
        <dd className="mono" title={`snapshot ${b.snapshot}`}>
          {b.base} <span className="muted">@ {b.snapshot}</span>
        </dd>
        {b.parent_branch && (
          <>
            <dt>{t("Forked from")}</dt>
            <dd className="mono">
              {onOpenParent ? (
                <button type="button" className="link-button mono" onClick={() => onOpenParent(b.parent_branch!)}>
                  {b.parent_branch}
                </button>
              ) : (
                b.parent_branch
              )}
              {b.parent_snapshot && <span className="muted"> @ {b.parent_snapshot}</span>}
            </dd>
          </>
        )}
        <dt>{t("Engine")}</dt>
        <dd>
          <Engine runtime={b.runtime} />
        </dd>
        <dt>{t("Endpoint")}</dt>
        <dd className="endpoint">
          {clustered && <span className="mono muted">{b.connection.host}:</span>}
          <span className="mono">{b.connection.port}</span>
          {ready && may("branch.connect") && <CopyButton label={t("Copy connection URL")} load={() => api.dsn(b.name)} />}
        </dd>
        {clustered && (
          <>
            <dt>{t("Node")}</dt>
            <dd className="mono">{b.node}</dd>
          </>
        )}
        <dt>{t("Owner")}</dt>
        <dd className="mono" title={b.owner ?? undefined}>{ownerLabel(b) ?? "—"}</dd>
        <dt>{t("Created")}</dt>
        <dd title={formatDateTime(b.created_at)}>{relative(b.created_at)}</dd>
        <dt>{t("TTL")}</dt>
        <dd>
          <TtlBar created={b.created_at} expires={b.expires_at} />
        </dd>
        <dt>{t("Storage")}</dt>
        <dd>
          <StorageBar storage={b.storage} max={max} />
        </dd>
      </dl>
      <div className="branch-actions" role="group" aria-label={t("Actions")}>
        {b.status === "lost" && !b.parent_branch && may("branch.recreate") && <button onClick={onRecreate}>{t("Recreate")}</button>}
        {b.proxy && engineLabel(b.runtime).engine !== "postgres" && may("branch.connect") && <button onClick={onCopyProxy}>{t("Copy proxy URL")}</button>}
        {onFork && may("branch.fork") && (b.status === "ready" || b.status === "paused") && b.checkpoints && <button onClick={onFork}>Fork</button>}
        {b.status === "ready" && may("branch.pause", "branch.update") && <button onClick={onPause}>Pause</button>}
        {b.status === "paused" && may("branch.resume", "branch.update") && <button onClick={onResume}>Resume</button>}
        {may("branch.extend") && (
          <>
            <button onClick={() => onExtend(extendTtl(b.expires_at, Date.now(), 60))}>{t("Extend 1h")}</button>
            <button onClick={() => onExtend(extendTtl(b.expires_at, Date.now(), 24 * 60))}>{t("Extend 24h")}</button>
            <button onClick={() => onExtend(extendTtl(b.expires_at, Date.now(), 168 * 60))}>{t("Extend 7d")}</button>
          </>
        )}
        {may("branch.reset") && (
          <button onClick={onReset} disabled={!ready}>
            {t("Reset")}
          </button>
        )}
        {may("branch.delete") && (
          <button className="danger-button" onClick={onDelete} disabled={!["ready", "paused", "failed", "lost"].includes(b.status)}>
            {t("Delete")}
          </button>
        )}
      </div>
      {sections.map((s) => (
        <section key={s.id} id={s.id} className="branch-section">
          <h2>{s.title}</h2>
          {s.body}
        </section>
      ))}
    </section>
  );
}
