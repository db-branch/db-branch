import { useEffect, useId, useReducer, useRef, useState } from "react";
import { api, Branch } from "../api";
import { confirmReducer, countdown, elapsed, expiryState, extendTtl, isBusy, longerChoices, ttlChoices } from "../lib/branches";
import { bytes, relative } from "../lib/format";
import { t } from "../lib/i18n";
import { splitOwned } from "../lib/branchname";
import { CheckpointPanel } from "./checkpoints";
import { ConnectPanel } from "./connect";
import { CopyButton, Engine, Icon, Status, StorageBar, TtlBar } from "./marks";
import { ErrorLine } from "./ui";

export type SnapInfo = { created_at: string; masked: boolean };

// Job: a branch this page is making right now (or failed to make).
export type Job = { name: string; base: string; runtime: string; startedAt: number; state: "running" | "failed"; error?: string };

// allowed: the session lists the actions it knows; one it does not list is
// left to the server, which decides for the branch's owner.
export const allowed = (can: Record<string, boolean> | undefined, action: string) => !can || !(action in can) || can[action];

// How long a "Confirm" stays armed before it goes back to the plain button.
const CONFIRM_MS = 8000;

function Elapsed({ since }: { since: number }) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);
  return <span className="mono">{elapsed(since, now)}</span>;
}

// BranchRow: one branch as a compact row (state, expiry, the two actions people
// use most) that opens to the connection, the facts and the rest of the actions.
export function BranchRow({
  branch: b,
  job,
  now,
  manage,
  can,
  snapshot,
  fresh,
  open,
  onToggle,
  onChange,
  onDismiss,
  tourAnchor,
}: {
  branch: Branch;
  job?: Job;
  now: number;
  manage: boolean;
  can?: Record<string, boolean>;
  snapshot?: SnapInfo;
  fresh?: boolean;
  open: boolean;
  onToggle: () => void;
  onChange: () => void;
  onDismiss?: () => void;
  tourAnchor?: boolean;
}) {
  const [confirm, ask] = useReducer(confirmReducer, null);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState("");
  const [working, setWorking] = useState(false);
  const detailsId = useId();
  const cancelButton = useRef<HTMLButtonElement>(null);
  const triggers = useRef<Record<string, HTMLButtonElement | null>>({});

  // An armed Confirm goes back by itself; focus moves to Cancel, the safe choice.
  useEffect(() => {
    if (!confirm) return;
    cancelButton.current?.focus();
    const id = setTimeout(() => ask({ type: "timeout" }), CONFIRM_MS);
    return () => clearTimeout(id);
  }, [confirm]);
  useEffect(() => {
    if (!note) return;
    const id = setTimeout(() => setNote(""), 4000);
    return () => clearTimeout(id);
  }, [note]);

  const run = (p: Promise<unknown>, done?: string) => {
    setWorking(true);
    return p
      .then(
        () => (setError(null), done && setNote(done), onChange()),
        (e: Error) => setError(e.message),
      )
      .finally(() => setWorking(false));
  };
  const cancel = () => {
    const was = confirm;
    ask({ type: "cancel" });
    if (was) requestAnimationFrame(() => triggers.current[was]?.focus());
  };

  const failedJob = job?.state === "failed";
  const failed = failedJob || b.status === "failed";
  const busy = !failed && (job?.state === "running" || isBusy(b.status));
  const ready = b.status === "ready" && !job;
  const ex = expiryState(b.expires_at, now);
  const soon = ready && ex === "soon";
  const expired = ready && ex === "expired";
  const noRole = t("Your role cannot do this.");
  const why = (action: string, needsReady = false) => (!allowed(can, action) ? noRole : needsReady && !ready ? t("Available once it is ready.") : undefined);
  const resetWhy = why("branch.reset", true);
  const extendWhy = why("branch.update");
  const deleteWhy = why("branch.delete");
  const [prefix, rest] = splitOwned(b.name, b.owner ?? undefined);
  const more = longerChoices(ttlChoices, b.expires_at, now);

  const chip = failed ? (
    <span className="chip chip-failed">{t("Failed")}</span>
  ) : busy ? (
    <span className="chip chip-busy">
      <span className="cap">{job || b.status === "provisioning" ? t("Provisioning") : b.status}</span>…
      {(job || b.status === "provisioning") && (
        <>
          {" "}
          <Elapsed since={job?.startedAt ?? Date.parse(b.created_at)} />
        </>
      )}
    </span>
  ) : (
    <span className={`chip chip-${ex}`} title={b.expires_at ? new Date(b.expires_at).toLocaleString() : undefined}>
      {ex === "soon" && <strong>{t("Expires soon")} · </strong>}
      {ex === "expired" ? t("Expired") : ex === "none" ? t("No expiry") : countdown(b.expires_at, now)}
    </span>
  );

  return (
    <li className={`brow ${soon ? "soon" : ""} ${expired ? "expired" : ""} ${failed ? "failed" : ""} ${fresh ? "fresh" : ""} ${open ? "open" : ""}`} data-tour={tourAnchor ? "branch" : undefined}>
      <div className="brow-main">
        <button type="button" className="brow-id" aria-expanded={open} aria-controls={detailsId} onClick={onToggle}>
          <span className="brow-line">
            <Status status={failed ? "failed" : b.status} reason={b.status_reason} tone={soon ? "soon" : expired ? "expired" : undefined} />
            <span className="mono strong brow-name">
              {prefix && <span className="muted">{prefix}</span>}
              {rest}
            </span>
            {b.access === "open" && <span className="badge">{t("open")}</span>}
            {chip}
          </span>
          <span className="brow-meta">
            <Engine runtime={b.runtime} />
            <span className="mono muted" title={t("Base")}>{b.base}</span>
            {b.storage && <span className="muted small" title={t("{own} own writes of {total} referenced", { own: bytes(b.storage.used), total: bytes(b.storage.referenced) })}>{bytes(b.storage.used)}</span>}
          </span>
        </button>
        <div className="brow-side">
          <span className="brow-actions">
            <CopyButton text={t("Copy URL")} label={t("Copy connection URL")} load={() => api.dsn(b.name)} disabled={!ready} />
            {manage && (
              <button
                type="button"
                className={soon || expired ? "primary" : ""}
                disabled={!ready || !!extendWhy || working}
                title={extendWhy ?? t("Adds 4 hours to the time left")}
                onClick={() => run(api.extendBranch(b.name, extendTtl(b.expires_at, now)), t("Extended by 4 hours"))}
              >
                {t("Extend +4h")}
              </button>
            )}
            <button type="button" className="icon-button toggle" aria-expanded={open} aria-controls={detailsId} aria-label={open ? t("Hide details") : t("Show details")} title={open ? t("Hide details") : t("Show details")} onClick={onToggle}>
              <Icon name={open ? "up" : "down"} />
            </button>
          </span>
        </div>
      </div>
      {(b.status_reason || failedJob) && (
        <p className="brow-reason" role={failedJob ? "alert" : undefined}>
          {failedJob ? job?.error : `${b.status}: ${b.status_reason}`}
          {failedJob && onDismiss && (
            <>
              {" "}
              <button type="button" className="text-button" onClick={onDismiss}>
                {t("Dismiss")}
              </button>
            </>
          )}
        </p>
      )}
      {note && (
        <p className="brow-note" role="status">
          {note}
        </p>
      )}
      <ErrorLine error={error} />
      {open && (
        <div className="brow-details" id={detailsId}>
          {fresh && ready && <p className="fresh-note">{t("Your Branch is ready. Connect with the details below.")}</p>}
          {ready ? <ConnectPanel branch={b.name} proxy={b.proxy} /> : <p className="muted small">{failed ? "" : t("Connection details appear once it is ready.")}</p>}
          <dl className="facts two">
            <dt>Snapshot</dt>
            <dd>
              <span className="mono">{b.snapshot}</span>
              {snapshot && <span className="muted"> · {relative(snapshot.created_at)}</span>}
              {snapshot?.masked && <span className="badge on">masked</span>}
            </dd>
            <dt>Node</dt>
            <dd className="mono">{b.node}</dd>
            <dt>Owner</dt>
            <dd>{b.owner ?? <span className="muted">—</span>}</dd>
            <dt>Created</dt>
            <dd title={new Date(b.created_at).toLocaleString()}>{relative(b.created_at)}</dd>
            <dt>Expires</dt>
            <dd className="wide">
              <span className="ttl-line">
                <TtlBar created={b.created_at} expires={b.expires_at} />
                {b.expires_at && <span className="muted small">{new Date(b.expires_at).toLocaleString()}</span>}
              </span>
            </dd>
            <dt>Storage</dt>
            <dd className="wide">
              {b.storage ? (
                <span className="storage-line">
                  <StorageBar storage={b.storage} max={b.storage.referenced} />
                  <span className="muted small">{t("{own} own writes of {total} referenced", { own: bytes(b.storage.used), total: bytes(b.storage.referenced) })}</span>
                </span>
              ) : (
                <span className="muted">—</span>
              )}
            </dd>
            {b.reset_at && (
              <>
                <dt>Reset</dt>
                <dd className="wide" title={new Date(b.reset_at).toLocaleString()}>
                  {relative(b.reset_at)}
                </dd>
              </>
            )}
          </dl>
          {manage && !failed && <CheckpointPanel branch={b} ready={ready} allow={(a) => allowed(can, a)} onChange={onChange} />}
          {manage ? (
            <div className="brow-manage">
              {more.length > 0 && ready && !extendWhy && (
                <div className="brow-keep">
                  <span className="field-label" id={`${detailsId}-keep`}>
                    {t("Keep for")}
                  </span>
                  <div className="segmented small" role="group" aria-labelledby={`${detailsId}-keep`}>
                    {more.map(([v, l]) => (
                      <button type="button" key={v} aria-pressed={false} disabled={working} onClick={() => run(api.extendBranch(b.name, v), t("Extended"))}>
                        {t(l)}
                      </button>
                    ))}
                  </div>
                </div>
              )}
              {confirm ? (
                <div
                  className="confirm"
                  role="group"
                  aria-label={confirm === "delete" ? t("Delete {name}?", { name: b.name }) : t("Reset {name}?", { name: b.name })}
                  onKeyDown={(e) => e.key === "Escape" && (e.stopPropagation(), cancel())}
                >
                  <span role="alert">
                    {confirm === "delete"
                      ? t("Delete {name}? Its data is lost. This cannot be undone.", { name: b.name })
                      : t("Reset {name} to the data of its Snapshot? Changes you made are lost. Checkpoints are kept.", { name: b.name })}
                  </span>
                  <span className="confirm-buttons">
                    <button type="button" ref={cancelButton} onClick={cancel}>
                      {t("Cancel")}
                    </button>
                    <button
                      type="button"
                      className="danger-button"
                      disabled={working}
                      onClick={() => {
                        const what = confirm;
                        ask({ type: "done" });
                        run(what === "delete" ? api.deleteBranch(b.name) : api.resetBranch(b.name), what === "reset" ? t("Reset started") : undefined);
                      }}
                    >
                      {confirm === "delete" ? t("Confirm delete") : t("Confirm reset")}
                    </button>
                  </span>
                </div>
              ) : (
                <div className="brow-danger">
                  <button type="button" ref={(el) => void (triggers.current.reset = el)} disabled={!!resetWhy || working} title={resetWhy ?? t("Back to the data it started with")} onClick={() => ask({ type: "ask", what: "reset" })}>
                    {t("Reset")}
                  </button>
                  <button type="button" ref={(el) => void (triggers.current.delete = el)} className="danger-button" disabled={!!deleteWhy || working} title={deleteWhy} onClick={() => ask({ type: "ask", what: "delete" })}>
                    {t("Delete")}
                  </button>
                </div>
              )}
            </div>
          ) : (
            <p className="muted small">{t("Sign in to extend, reset or delete.")}</p>
          )}
        </div>
      )}
    </li>
  );
}
