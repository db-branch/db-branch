import { useEffect, useRef, useState } from "react";
import { api, Branch, MergeOperation, MergeResult } from "../api";
import { LocalizedError, serverError } from "../lib/errors";
import { t } from "../lib/i18n";
import { Modal } from "./modal";
import { ErrorLine } from "./ui";

export function MergeBranchModal({
  branch,
  checkpoint,
  sourceLine,
  onClose,
  onMerged,
}: {
  branch: Branch;
  checkpoint: string;
  sourceLine: number;
  onClose: () => void;
  onMerged?: () => void;
}) {
  const [allowDDL, setAllowDDL] = useState(false);
  const [ignoreTriggers, setIgnoreTriggers] = useState(false);
  const [busy, setBusy] = useState(false);
  const [applying, setApplying] = useState(false);
  const [phase, setPhase] = useState("");
  const [result, setResult] = useState<MergeResult | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [previewTick, setPreviewTick] = useState(0);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const pollOperation = async (initial: MergeOperation, alive: () => boolean): Promise<MergeOperation | null> => {
    let cur = initial;
    while (cur.state === "running" && alive()) {
      if (cur.phase) setPhase(cur.phase);
      await new Promise((r) => setTimeout(r, 350));
      if (!alive()) return null;
      cur = await api.mergeOperation(cur.id);
    }
    return alive() ? cur : null;
  };

  useEffect(() => {
    let active = true;
    const alive = () => active;
    setBusy(true);
    setApplying(false);
    setError(null);
    setResult(null);
    setPhase("");
    (async () => {
      try {
        const started = await api.mergeBranch(branch.name, {
          checkpoint,
          dry_run: true,
          allow_ddl: allowDDL,
          ignore_triggers: ignoreTriggers,
        });
        const done = await pollOperation(started, alive);
        if (!done || !active) return;
        if (done.result) setResult(done.result);
        if (done.state === "failed" && !done.result) setError(done.error ? serverError(done.error) : new LocalizedError(t("Preview failed.")));
        else if (done.result?.error && done.result.status !== "conflicted") setError(serverError(done.result.error, done.result.code));
      } catch (e) {
        if (active) setError(e);
      } finally {
        if (active) setBusy(false);
      }
    })();
    return () => {
      active = false;
    };
  }, [branch.name, checkpoint, allowDDL, ignoreTriggers, previewTick]);

  const applyMerge = async () => {
    if (busy) return;
    setBusy(true);
    setApplying(true);
    setError(null);
    setPhase("");
    try {
      const started = await api.mergeBranch(branch.name, {
        checkpoint,
        dry_run: false,
        allow_ddl: allowDDL,
        ignore_triggers: ignoreTriggers,
      });
      const done = await pollOperation(started, () => mounted.current);
      if (!done || !mounted.current) return;
      if (done.result) setResult(done.result);
      if (done.state === "failed") {
        setError(done.result?.error ? serverError(done.result.error, done.result.code) : done.error ? serverError(done.error) : new LocalizedError(t("Merge failed.")));
      } else if (done.result?.status === "merged") {
        onMerged?.();
      }
    } catch (e) {
      if (mounted.current) setError(e);
    } finally {
      if (mounted.current) {
        setBusy(false);
        setApplying(false);
      }
    }
  };

  const conflicts = result?.conflicts ?? [];
  const totalConflicts = result?.total_conflicts ?? conflicts.length;
  const hasConflicts = totalConflicts > 0 || result?.status === "conflicted";
  const applied = result?.status === "merged";

  return (
    <Modal title={t("Merge into {name}", { name: branch.name })} onClose={onClose}>
      <div className="form">
        <p className="small muted">
          {t("Merge changes from this checkpoint's line into the live branch.")} ({checkpoint} · line {sourceLine})
        </p>

        {busy && (
          <p className="small">
            {applying ? t("Applying merge to live branch...") : t("Calculating 3-way diff...")}
            {phase ? ` (${phase})` : ""}
          </p>
        )}

        <ErrorLine error={error} />

        {result && (
          <div className="cp-merge-summary">
            <div className="cp-main">
              <span className={applied ? "badge on" : hasConflicts ? "badge err" : "badge"}>{result.status}</span>
              <span className="mono small">
                +{result.inserted} ~{result.updated} -{result.deleted}
                {result.ddl ? ` · DDL ${result.ddl}` : ""}
              </span>
              {result.ancestor && <span className="muted small">ancestor {result.ancestor}</span>}
            </div>

            {result.tables && result.tables.length > 0 ? (
              <ul className="cp-merge-tables small mono">
                {result.tables.map((tb) => (
                  <li key={`${tb.database}.${tb.table}`}>
                    {tb.database}.{tb.table}: +{tb.inserted} ~{tb.updated} -{tb.deleted}
                    {tb.ddl ? " (DDL)" : ""}
                  </li>
                ))}
              </ul>
            ) : (
              !hasConflicts &&
              !busy && (
                <p className="small muted">
                  {t("No changes to apply.")}
                </p>
              )
            )}

            {result.checkpoint && applied && (
              <p className="small">{t("Automatic checkpoint saved: {checkpoint}", { checkpoint: result.checkpoint })}</p>
            )}

            {hasConflicts && (
              <div className="cp-merge-conflicts">
                <p className="small err">{t("Resolve all conflicts before applying the merge.")}</p>
                <ul className="small mono">
                  {conflicts.map((c, idx) => (
                    <li key={idx}>
                      {c.type} {c.database}.{c.table}
                      {c.message ? `: ${c.message}` : ""}
                    </li>
                  ))}
                </ul>
              </div>
            )}
          </div>
        )}

        {!applied && (
          <div className="cp-merge-opts small">
            <label>
              <input type="checkbox" checked={allowDDL} disabled={busy} onChange={(e) => setAllowDDL(e.target.checked)} /> {t("Allow DDL")}
            </label>{" "}
            <label>
              <input type="checkbox" checked={ignoreTriggers} disabled={busy} onChange={(e) => setIgnoreTriggers(e.target.checked)} /> {t("Ignore triggers")}
            </label>
          </div>
        )}

        <div className="form-actions">
          {!applied && (
            <>
              <button type="button" disabled={busy} onClick={() => setPreviewTick((n) => n + 1)}>
                {t("Preview again")}
              </button>
              <button type="button" className="primary" disabled={busy || !result || hasConflicts} onClick={applyMerge}>
                {t("Apply Merge")}
              </button>
            </>
          )}
          <button type="button" onClick={onClose}>
            {t("Close")}
          </button>
        </div>
      </div>
    </Modal>
  );
}
