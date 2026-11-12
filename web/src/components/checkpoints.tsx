import { FormEvent, useEffect, useMemo, useState } from "react";
import { api, Branch, HistoryTx } from "../api";
import { useCapabilities } from "../lib/caps";
import { checkpointNameProblem, hasHistory, MAX_CHECKPOINT_NAME, newestFirst, notes, savedLabel } from "../lib/checkpoints";
import { bytes, relative } from "../lib/format";
import { hasHistoryModule, rowsLabel, txNumber } from "../lib/history";
import { GNode } from "../lib/historygraph";
import { t } from "../lib/i18n";
import { useLive } from "../lib/live";
import { HistoryGraph } from "./historygraph";
import { ErrorLine } from "./ui";

// How long an armed Confirm stays before it goes back to the plain button.
const CONFIRM_MS = 8000;

type Asking = { what: "restore" | "delete"; name: string; from: "list" | "graph" } | null;

// How often the transactions on the graph are read again while it is shown.
const EVENTS_MS = 15000;

// CheckpointPanel: save the branch's state, see the saved states, go back to
// one or delete it. Restoring loses nothing, and the panel says so before it
// asks to confirm. allow tells which actions the signed-in person may do.
export function CheckpointPanel({ branch: b, ready, allow, onChange }: { branch: Branch; ready: boolean; allow: (action: string) => boolean; onChange?: () => void }) {
  const { data: graph, error: loadError, reload } = useLive(() => api.checkpointGraph(b.name), ["branch", "operation"]);
  const list = graph?.items;
  const caps = useCapabilities();
  // The transactions are the history module's, and only the owner and admins may read them.
  const overlay = hasHistoryModule(caps) && allow("ext.history.read");
  const [events, setEvents] = useState<Record<number, HistoryTx[]>>();
  const [selected, setSelected] = useState<GNode | null>(null);
  const [askingTx, setAskingTx] = useState(false);
  const [name, setName] = useState("");
  const [asking, setAsking] = useState<Asking>(null);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState("");
  const [working, setWorking] = useState(false);
  const problem = checkpointNameProblem(name);
  const noRole = t("Your role cannot do this.");
  const why = (action: string) => (!allow(action) ? noRole : !ready ? t("Available once it is ready.") : undefined);
  const saveWhy = why("branch.checkpoint");
  const restoreWhy = why("branch.restore");
  const deleteWhy = why("branch.checkpoint_delete");

  const shape = useMemo(() => JSON.stringify([graph?.lines, graph?.items.map((c) => c.name)]), [graph]);
  useEffect(() => setSelected(null), [shape]);
  useEffect(() => {
    if (!overlay || !graph) return;
    let live = true;
    const load = () =>
      Promise.all(graph.lines.map((l) => api.historyEvents(b.name, l.line).then((r) => [l.line, r.items.slice().reverse()] as const, () => null))).then((rs) => {
        const ok = rs.filter((r) => r !== null);
        if (live) setEvents(ok.length > 0 ? Object.fromEntries(ok) : undefined);
      });
    load();
    const id = setInterval(load, EVENTS_MS);
    return () => {
      live = false;
      clearInterval(id);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [overlay, b.name, shape]);
  useEffect(() => {
    if (!asking && !askingTx) return;
    const id = setTimeout(() => (setAsking(null), setAskingTx(false)), CONFIRM_MS);
    return () => clearTimeout(id);
  }, [asking, askingTx]);
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
    run(api.createCheckpoint(b.name, label || undefined), label ? t(savedLabel(label)) : t("Saved checkpoint")).then(() => setName(""));
  };

  const rows = list ? newestFirst(list) : [];
  const lines = hasHistory(rows);
  const dots = !!events && Object.values(events).some((l) => l.length > 0);
  const select = (n: GNode) => (setSelected(n), setAsking(null), setAskingTx(false));
  const txRestoreWhy = !allow("ext.history.write") ? noRole : !ready ? t("Available once it is ready.") : undefined;

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
      <ErrorLine error={error ?? loadError} />
      {graph && (rows.length > 0 || graph.lines.length > 1) && (
        <>
          <HistoryGraph branch={b.name} lines={graph.lines} checkpoints={graph.items} head={graph.head} events={events} selected={selected?.id ?? null} onSelect={select} />
          <p className="muted small hg-legend">
            {dots ? t("Dots are recorded transactions. Select a checkpoint or a dot.") : t("Select a checkpoint.")}
          </p>
          {selected && (
            <div className="hg-detail" role="region" aria-label="Selected" aria-live="polite">
              {selected.kind === "tx" && selected.tx ? (
                <TxDetail
                  tx={selected.tx}
                  line={selected.line}
                  asking={askingTx}
                  working={working}
                  why={txRestoreWhy}
                  branch={b.name}
                  onAsk={setAskingTx}
                  onRestore={() => {
                    setAskingTx(false);
                    run(api.historyRestore(b.name, { line: selected.line, gtid: selected.tx!.gtid }), t("Restore started"));
                  }}
                />
              ) : selected.kind === "checkpoint" && selected.checkpoint ? (
                <CheckpointDetail
                  c={selected.checkpoint}
                  branch={b.name}
                  asking={asking?.from === "graph" && asking.name === selected.checkpoint.name}
                  working={working}
                  why={restoreWhy}
                  onAsk={(on) => setAsking(on ? { what: "restore", name: selected.checkpoint!.name, from: "graph" } : null)}
                  onRestore={() => {
                    const cp = selected.checkpoint!.name;
                    setAsking(null);
                    run(api.restoreCheckpoint(b.name, cp), t("Restore started"));
                  }}
                />
              ) : (
                <p className="small">{t("The branch's creation state. Reset returns here.")}</p>
              )}
            </div>
          )}
        </>
      )}
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
              {asking?.name === c.name && asking.from === "list" ? (
                <div className="confirm" role="group" aria-label={asking.what === "restore" ? `Restore ${c.name}` : `Delete ${c.name}`} onKeyDown={(e) => e.key === "Escape" && (e.stopPropagation(), setAsking(null))}>
                  <span role="alert">
                    {asking.what === "restore"
                      ? t("Restore {name} to {checkpoint}? Nothing is lost: the current state is saved first as a before-restore checkpoint, and every other checkpoint stays.", { name: b.name, checkpoint: c.name })
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
                  <button type="button" disabled={!!restoreWhy || working} title={restoreWhy ?? t("Go back to this state; nothing is lost")} onClick={() => setAsking({ what: "restore", name: c.name, from: "list" })}>
                    Restore
                  </button>
                  <button type="button" className="text-button danger" disabled={!!deleteWhy || working} title={deleteWhy} onClick={() => setAsking({ what: "delete", name: c.name, from: "list" })}>
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

// The confirmation of a restore: what it does, in a sentence, before it is done.
function Confirm({ label, text, action, danger, working, onCancel, onConfirm }: { label: string; text: string; action: string; danger?: boolean; working: boolean; onCancel: () => void; onConfirm: () => void }) {
  return (
    <div className="confirm" role="group" aria-label={label} onKeyDown={(e) => e.key === "Escape" && (e.stopPropagation(), onCancel())}>
      <span role="alert">{text}</span>
      <span className="confirm-buttons">
        <button type="button" autoFocus onClick={onCancel}>
          {t("Cancel")}
        </button>
        <button type="button" className={danger ? "danger-button" : "primary"} disabled={working} onClick={onConfirm}>
          {action}
        </button>
      </span>
    </div>
  );
}

function CheckpointDetail({ c, branch, asking, working, why, onAsk, onRestore }: { c: import("../api").Checkpoint; branch: string; asking: boolean; working: boolean; why?: string; onAsk: (on: boolean) => void; onRestore: () => void }) {
  return (
    <div className="hg-detail-body">
      <div className="cp-main">
        <span className="mono strong">{c.name}</span>
        {notes(c).map((n) => (
          <span key={n} className={n === "current" ? "badge on" : "badge"}>
            {n}
          </span>
        ))}
        <span className="muted small">line {c.line}</span>
        <span className="muted small" title={new Date(c.created_at).toLocaleString()}>
          {relative(c.created_at)}
        </span>
        <span className="muted small">{bytes(c.size)}</span>
      </div>
      {asking ? (
        <Confirm
          label={`Restore ${c.name}`}
          text={t("Restore {name} to {checkpoint}? Nothing is lost: the current state is saved first as a before-restore checkpoint, and every other checkpoint stays.", { name: branch, checkpoint: c.name })}
          action="Confirm restore"
          working={working}
          onCancel={() => onAsk(false)}
          onConfirm={onRestore}
        />
      ) : (
        <button type="button" disabled={!!why || working} title={why ?? t("Go back to this state; nothing is lost")} onClick={() => onAsk(true)}>
          Restore
        </button>
      )}
    </div>
  );
}

function TxDetail({ tx, line, branch, asking, working, why, onAsk, onRestore }: { tx: HistoryTx; line: number; branch: string; asking: boolean; working: boolean; why?: string; onAsk: (on: boolean) => void; onRestore: () => void }) {
  return (
    <div className="hg-detail-body">
      <div className="cp-main">
        <span className="mono strong">#{txNumber(tx.gtid)}</span>
        <span className="muted small">line {line}</span>
        <span className="muted small" title={new Date(tx.time).toLocaleString()}>
          {relative(tx.time)}
        </span>
        <span className="small mono">{rowsLabel(tx)}</span>
      </div>
      {tx.sql && (
        <pre className="hg-sql mono small" aria-label="Statements">
          {tx.sql}
        </pre>
      )}
      {tx.tables.length > 0 && (
        <p className="muted small mono">
          {tx.tables.map((x) => `${x.name} ${rowsLabel({ ...x, ddl: false })}`).join(" · ")}
        </p>
      )}
      {asking ? (
        <Confirm
          label="Restore to here"
          text={t("Restore {name} to just after this transaction? Nothing is lost: the branch continues on a new line, the current state is saved first as a before-restore checkpoint, and every checkpoint stays.", { name: branch })}
          action="Confirm restore"
          working={working}
          onCancel={() => onAsk(false)}
          onConfirm={onRestore}
        />
      ) : (
        <button type="button" disabled={!!why || working} title={why ?? t("Go back to just after this transaction; nothing is lost")} onClick={() => onAsk(true)}>
          Restore to here
        </button>
      )}
    </div>
  );
}
