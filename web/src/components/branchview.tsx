import type { ReactNode } from "react";
import { api, Branch } from "../api";
import { relative } from "../lib/format";
import { t } from "../lib/i18n";
import { BranchName } from "./branchname";
import { CopyButton, Engine, Status, StorageBar, TtlBar } from "./marks";
import { RecordingBadge } from "./recording";

export type BranchSection = { id: string; title: string; body: ReactNode };

// BranchView is the page of one branch: what it is, the actions on it, and the
// sections the page gives it (connect, checkpoints, upgrade, history). The
// actions are the ones the list used to have in its menu.
export function BranchView({
  b,
  clustered,
  max,
  sections = [],
  onBack,
  onExtend,
  onReset,
  onRecreate,
  onDelete,
  onCopyProxy,
}: {
  b: Branch;
  clustered: boolean;
  max: number;
  sections?: BranchSection[];
  onBack: () => void;
  onExtend: (ttl: string) => void;
  onReset: () => void;
  onRecreate: () => void;
  onDelete: () => void;
  onCopyProxy: () => void;
}): ReactNode {
  const ready = b.status === "ready";
  return (
    <section className="page branch-page">
      <button type="button" className="text-button back-link" onClick={onBack}>
        ← {t("Back to Branches")}
      </button>
      <header className="page-head">
        <h1>
          <BranchName name={b.name} owner={b.owner} />
        </h1>
        <Status status={b.status} reason={b.status_reason} />
        {b.access === "open" && (
          <span className="badge" title={t("Anyone may connect, with the shared password")}>
            open
          </span>
        )}
        <RecordingBadge record={b.record} />
      </header>
      {b.status_reason && <p className="reason">{b.status_reason}</p>}
      <dl className="branch-facts">
        <dt>{t("Base")}</dt>
        <dd className="mono" title={`snapshot ${b.snapshot}`}>
          {b.base} <span className="muted">@ {b.snapshot}</span>
        </dd>
        <dt>{t("Engine")}</dt>
        <dd>
          <Engine runtime={b.runtime} />
        </dd>
        <dt>{t("Endpoint")}</dt>
        <dd className="endpoint">
          {clustered && <span className="mono muted">{b.connection.host}:</span>}
          <span className="mono">{b.connection.port}</span>
          {ready && <CopyButton label={t("Copy connection URL")} load={() => api.dsn(b.name)} />}
        </dd>
        {clustered && (
          <>
            <dt>{t("Node")}</dt>
            <dd className="mono">{b.node}</dd>
          </>
        )}
        <dt>{t("Owner")}</dt>
        <dd className="mono">{b.owner ?? "—"}</dd>
        <dt>{t("Created")}</dt>
        <dd title={new Date(b.created_at).toLocaleString()}>{relative(b.created_at)}</dd>
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
        {b.status === "lost" && <button onClick={onRecreate}>{t("Recreate")}</button>}
        {b.proxy && <button onClick={onCopyProxy}>{t("Copy proxy URL")}</button>}
        <button onClick={() => onExtend("1h")}>{t("Extend 1h")}</button>
        <button onClick={() => onExtend("24h")}>{t("Extend 24h")}</button>
        <button onClick={() => onExtend("168h")}>{t("Extend 7d")}</button>
        <button onClick={onReset} disabled={!ready}>
          {t("Reset")}
        </button>
        <button className="danger-button" onClick={onDelete} disabled={!["ready", "failed", "lost"].includes(b.status)}>
          {t("Delete")}
        </button>
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
