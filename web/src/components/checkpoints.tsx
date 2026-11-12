import { FormEvent, useEffect, useState } from "react";
import { api, Branch } from "../api";
import { checkpointNameProblem, hasHistory, MAX_CHECKPOINT_NAME, newestFirst, notes, savedLabel, spaceWarning } from "../lib/checkpoints";
import { bytes, relative } from "../lib/format";
import { t } from "../lib/i18n";
import { useLive } from "../lib/live";
import { ErrorLine } from "./ui";

// How long an armed Confirm stays before it goes back to the plain button.
const CONFIRM_MS = 8000;

type Asking = { what: "restore" | "delete"; name: string } | null;

// CheckpointPanel: save the branch's state, see the saved states, go back to
// one or delete it. Restoring loses nothing, and the panel says so before it
// asks to confirm. allow tells which actions the signed-in person may do.
export function CheckpointPanel({ branch: b, ready, allow, onChange }: { branch: Branch; ready: boolean; allow: (action: string) => boolean; onChange?: () => void }) {
  const { data: list, error: loadError, reload } = useLive(() => api.checkpoints(b.name), ["branch", "operation"]);
  const [name, setName] = useState("");
  const [asking, setAsking] = useState<Asking>(null);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState("");
  const [warning, setWarning] = useState("");
  const [working, setWorking] = useState(false);
  const problem = checkpointNameProblem(name);
  const noRole = t("Your role cannot do this.");
  const why = (action: string) => (!allow(action) ? noRole : !ready ? t("Available once it is ready.") : undefined);
  const saveWhy = why("branch.checkpoint");
  const restoreWhy = why("branch.restore");
  const deleteWhy = why("branch.checkpoint_delete");

  useEffect(() => {
    if (!asking) return;
    const id = setTimeout(() => setAsking(null), CONFIRM_MS);
    return () => clearTimeout(id);
  }, [asking]);
  useEffect(() => {
    if (!note) return;
    const id = setTimeout(() => setNote(""), 4000);
    return () => clearTimeout(id);
  }, [note]);

  const run = (p: Promise<unknown>, done: string) => {
    setWorking(true);
    return p
      .then(
        () => (setError(null), setNote(done), reload(), onChange?.()),
        (e: Error) => setError(e.message),
      )
      .finally(() => setWorking(false));
  };

  const save = (e: FormEvent) => {
    e.preventDefault();
    if (problem || saveWhy) return;
    const label = name.trim();
    setWarning("");
    run(
      api.createCheckpoint(b.name, label || undefined).then((cp) => setWarning(spaceWarning(cp))),
      label ? t(savedLabel(label)) : t("Saved checkpoint"),
    ).then(() => setName(""));
  };

  const rows = list ? newestFirst(list) : [];
  const lines = hasHistory(rows);

  return (
    <section className="cps" aria-label="Checkpoints">
      <h3 className="cps-title">Checkpoints</h3>
      <p className="muted small">{t("Save the state of this Branch before risky work. Restoring one loses nothing.")}</p>
      <form className="cps-save" onSubmit={save}>
        <input
          className="mono"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="Name (optional)"
          aria-label="Name (optional)"
          maxLength={MAX_CHECKPOINT_NAME + 10}
          aria-invalid={problem ? true : undefined}
          disabled={!!saveWhy}
        />
        <button type="submit" disabled={!!saveWhy || !!problem || working} title={saveWhy}>
          Save checkpoint
        </button>
      </form>
      {problem && <p className="field-error small">{problem === "long" ? t("At most {max} characters.", { max: MAX_CHECKPOINT_NAME }) : t("Use lowercase letters, digits and hyphens, not at the ends.")}</p>}
      {note && (
        <p className="brow-note" role="status">
          {note}
        </p>
      )}
      {warning && (
        <p className="cps-warning small" role="status">
          {warning}
        </p>
      )}
      <ErrorLine error={error ?? loadError} />
      {list && rows.length === 0 && <p className="muted small">{t("No checkpoints yet.")}</p>}
      {rows.length > 0 && (
        <ul className="cps-list">
          {rows.map((c) => (
            <li key={c.name} className={`cp ${c.current ? "current" : ""}`}>
              <div className="cp-main">
                <span className="mono strong cp-name">{c.name}</span>
                {notes(c).map((n) => (
                  <span key={n} className={n === "current" ? "badge on" : "badge"} title={n === "current" ? t("The branch's data descends from this checkpoint.") : t("Saved automatically before a restore.")}>
                    {n}
                  </span>
                ))}
                <span className="muted small" title={new Date(c.created_at).toLocaleString()}>
                  {relative(c.created_at)}
                </span>
                <span className="muted small" title={t("Space only this checkpoint holds")}>
                  {bytes(c.size)}
                </span>
                {lines && <span className="muted small">line {c.line}</span>}
              </div>
              {asking?.name === c.name ? (
                <div className="confirm" role="group" aria-label={asking.what === "restore" ? `Restore ${c.name}` : `Delete ${c.name}`} onKeyDown={(e) => e.key === "Escape" && (e.stopPropagation(), setAsking(null))}>
                  <span role="alert">
                    {asking.what === "restore"
                      ? t("Restore {name} to {checkpoint}? Nothing is lost: the current state is saved first as a before-restore checkpoint (the newest five are kept), and every other checkpoint stays.", { name: b.name, checkpoint: c.name })
                      : t("Delete checkpoint {checkpoint}? The other checkpoints are not affected.", { checkpoint: c.name })}
                  </span>
                  <span className="confirm-buttons">
                    <button type="button" autoFocus onClick={() => setAsking(null)}>
                      {t("Cancel")}
                    </button>
                    <button
                      type="button"
                      className={asking.what === "delete" ? "danger-button" : "primary"}
                      disabled={working}
                      onClick={() => {
                        const what = asking.what;
                        setAsking(null);
                        run(what === "restore" ? api.restoreCheckpoint(b.name, c.name) : api.deleteCheckpoint(b.name, c.name), what === "restore" ? t("Restore started") : t("Deleted checkpoint"));
                      }}
                    >
                      {asking.what === "restore" ? "Confirm restore" : "Confirm delete"}
                    </button>
                  </span>
                </div>
              ) : (
                <span className="cp-actions">
                  <button type="button" disabled={!!restoreWhy || working} title={restoreWhy ?? t("Go back to this state; nothing is lost")} onClick={() => setAsking({ what: "restore", name: c.name })}>
                    Restore
                  </button>
                  <button type="button" className="text-button danger" disabled={!!deleteWhy || working} title={deleteWhy} onClick={() => setAsking({ what: "delete", name: c.name })}>
                    Delete
                  </button>
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
