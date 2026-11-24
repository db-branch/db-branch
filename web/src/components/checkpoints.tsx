import { FormEvent, ReactNode, RefObject, useEffect, useMemo, useRef, useState } from "react";
import { api, Branch, HistoryTx } from "../api";
import { useCapabilities } from "../lib/caps";
import { checkpointNameProblem, MAX_CHECKPOINT_NAME, newestFirst, notes, savedLabel, spaceWarning } from "../lib/checkpoints";
import { bytes, relative } from "../lib/format";
import { hasHistoryModule, rowsLabel, txNumber } from "../lib/history";
import { GNode, mergeLines } from "../lib/historygraph";
import { t } from "../lib/i18n";
import { useLive } from "../lib/live";
import { HistoryGraph } from "./historygraph";
import { Icon } from "./marks";
import { MergeBranchModal } from "./mergebranch";
import { Modal } from "./modal";
import { ErrorLine } from "./ui";

// How long an armed Confirm stays before it goes back to the plain button.
const CONFIRM_MS = 8000;

// How much larger the graph is drawn in the modal.
const MODAL_SCALE = 1.5;

type Asking = { what: "restore" | "delete"; name: string; from: "list" | "graph" } | null;

// How often the transactions on the graph are read again while it is shown.
const EVENTS_MS = 15000;

// usePanel holds everything the Checkpoints panel knows and does: the data, the
// selection, the pending confirmations and the actions. The panel is drawn from
// it twice while it is enlarged (on the page and in the modal), so both always
// show the same thing, and an action in one refreshes the other.
function usePanel({ branch: b, ready, allow, onChange }: { branch: Branch; ready: boolean; allow: (action: string) => boolean; onChange?: () => void }) {
  const { data: graph, error: loadError, reload } = useLive(() => api.checkpointGraph(b.name), ["branch", "operation"]);
  const caps = useCapabilities();
  // The transactions are the history module's, and only the owner and admins may read them.
  const overlay = hasHistoryModule(caps) && allow("ext.history.read");
  const [events, setEvents] = useState<Record<number, HistoryTx[]>>();
  const [fromTx, setFromTx] = useState<Record<number, string>>();
  const [selected, setSelected] = useState<GNode | null>(null);
  const [askingTx, setAskingTx] = useState(false);
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
  const deleteWhy = !allow("branch.checkpoint.delete") && !allow("branch.checkpoint_delete") ? noRole : !ready ? t("Available once it is ready.") : undefined;
  const forkWhy = !allow("branch.fork") ? noRole : b.status !== "ready" && b.status !== "paused" ? t("Available once it is ready.") : undefined;
  const mergeWhy = !allow("branch.merge") ? noRole : b.status !== "ready" && b.status !== "paused" ? t("Available once it is ready.") : undefined;

  const shape = useMemo(() => JSON.stringify([graph?.lines, graph?.items.map((c) => c.name)]), [graph]);
  useEffect(() => setSelected(null), [shape]);
  useEffect(() => {
    if (!overlay || !graph) return;
    let live = true;
    const load = () =>
      Promise.all(graph.lines.map((l) => api.historyEvents(b.name, l.line).then((r) => [l.line, r.items.slice().reverse(), r.from_tx] as const, () => null))).then((rs) => {
        const ok = rs.filter((r) => r !== null);
        if (!live) return;
        setEvents(ok.length > 0 ? Object.fromEntries(ok.map(([n, items]) => [n, items])) : undefined);
        setFromTx(Object.fromEntries(ok.filter(([, , f]) => !!f).map(([n, , f]) => [n, f!])));
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

  const saveNow = () => {
    if (problem || saveWhy) return;
    const label = name.trim();
    setWarning("");
    run(
      api.createCheckpoint(b.name, label || undefined).then((cp) => setWarning(spaceWarning(cp))),
      label ? t(savedLabel(label)) : t("Saved checkpoint"),
    ).then(() => setName(""));
  };
  const save = (e: FormEvent) => {
    e.preventDefault();
    saveNow();
  };

  const rows = graph ? newestFirst(graph.items) : [];
  // The line a node is drawn on: a line that continued another one's lane is shown as that one.
  const lanes = useMemo(() => (graph ? mergeLines({ lines: graph.lines, checkpoints: graph.items, head: graph.head, events, recorded: b.record, fromTx }) : new Map<number, number>()), [graph, events, fromTx, b.record]);

  return {
    branch: b,
    graph,
    rows,
    events,
    fromTx,
    error: error ?? loadError,
    note,
    warning,
    working,
    name,
    setName,
    problem,
    saveWhy,
    restoreWhy,
    deleteWhy,
    forkWhy,
    mergeWhy,
    txRestoreWhy: !allow("ext.history.write") ? noRole : !ready ? t("Available once it is ready.") : undefined,
    save,
    // saveFirst: the button of a restore confirmation, which saves a checkpoint (named as typed in the form, or by time).
    saveFirst: saveNow,
    selected,
    select: (n: GNode) => (setSelected(n), setAsking(null), setAskingTx(false)),
    asking,
    setAsking,
    askingTx,
    setAskingTx,
    restoreCheckpoint: (cp: string): void => {
      setAsking(null);
      void run(api.restoreCheckpoint(b.name, cp), t("Restore started"));
    },
    deleteCheckpoint: (cp: string): void => {
      setAsking(null);
      void run(api.deleteCheckpoint(b.name, cp), t("Deleted checkpoint"));
    },
    restoreTx: (line: number, gtid: string): void => {
      setAskingTx(false);
      void run(api.historyRestore(b.name, { line, gtid }), t("Restore started"));
    },
    // shown: the line number to tell the person for a line of the server.
    shown: (line: number) => lanes.get(line) ?? line,
    // showLines: the lines are worth naming once there is more than one lane.
    showLines: new Set(lanes.values()).size > 1,
  };
}

export type Panel = ReturnType<typeof usePanel>;

// CheckpointPanel: save the branch's state, see the saved states, go back to
// one or delete it. A restore saves nothing by itself, and the panel says so before it
// asks to confirm. allow tells which actions the signed-in person may do. The
// graph has an Expand button that opens the whole panel in a modal.
export function CheckpointPanel(props: { branch: Branch; ready: boolean; allow: (action: string) => boolean; onChange?: () => void; onFork?: (checkpoint: string) => void }) {
  const p = usePanel(props);
  const [open, setOpen] = useState(false);
  const opener = useRef<HTMLButtonElement>(null);
  return <PanelView p={p} open={open} onOpen={() => setOpen(true)} onClose={() => setOpen(false)} expandRef={opener} onFork={props.onFork} />;
}

// PanelView draws the panel on the page and, while open, the same panel
// enlarged in a modal. The one on the page is inert meanwhile: out of reach of
// the keyboard and of screen readers, so there is one of each control to find.
export function PanelView({ p, open, onOpen, onClose, expandRef, onFork }: { p: Panel; open: boolean; onOpen: () => void; onClose: () => void; expandRef?: RefObject<HTMLButtonElement | null>; onFork?: (checkpoint: string) => void }) {
  const [mergeTarget, setMergeTarget] = useState<{ checkpoint: string; line: number } | null>(null);
  return (
    <>
      <section className="cps" aria-label="Checkpoints" inert={open}>
        <PanelBody p={p} onExpand={onOpen} expandRef={expandRef} onFork={onFork} onMerge={(checkpoint, line) => setMergeTarget({ checkpoint, line })} />
      </section>
      {open && (
        <Modal title={t("Checkpoints of {name}", { name: p.branch.name })} onClose={onClose} returnFocus={expandRef}>
          <section className="cps cps-modal" aria-label="Checkpoints">
            <PanelBody p={p} modal onFork={onFork ? (cp) => (onClose(), onFork(cp)) : undefined} onMerge={(checkpoint, line) => (onClose(), setMergeTarget({ checkpoint, line }))} />
          </section>
        </Modal>
      )}
      {mergeTarget && (
        <MergeBranchModal
          branch={p.branch}
          checkpoint={mergeTarget.checkpoint}
          sourceLine={mergeTarget.line}
          onClose={() => setMergeTarget(null)}
        />
      )}
    </>
  );
}

// PanelBody is the panel itself: save, the graph with the selected node and its
// actions, and the list. modal draws it enlarged for the modal.
export function PanelBody({ p, modal, onExpand, expandRef, onFork, onMerge }: { p: Panel; modal?: boolean; onExpand?: () => void; expandRef?: RefObject<HTMLButtonElement | null>; onFork?: (checkpoint: string) => void; onMerge?: (checkpoint: string, line: number) => void }): ReactNode {
  const { branch: b, graph, rows, selected, asking, working } = p;
  const [localMerge, setLocalMerge] = useState<{ checkpoint: string; line: number } | null>(null);
  const openMerge = onMerge ?? ((checkpoint: string, line: number) => setLocalMerge({ checkpoint, line }));
  const dots = !!p.events && Object.values(p.events).some((l) => l.length > 0);
  return (
    <>
      {!modal && <h3 className="cps-title">Checkpoints</h3>}
      <p className="muted small">{t("Save the state of this Branch before risky work. A restore replaces the current state and saves nothing.")}</p>
      <form className="cps-save" onSubmit={p.save}>
        <input
          className="mono"
          value={p.name}
          onChange={(e) => p.setName(e.target.value)}
          placeholder="Name (optional)"
          aria-label="Name (optional)"
          maxLength={MAX_CHECKPOINT_NAME + 10}
          aria-invalid={p.problem ? true : undefined}
          disabled={!!p.saveWhy}
        />
        <button type="submit" disabled={!!p.saveWhy || !!p.problem || working} title={p.saveWhy}>
          Save checkpoint
        </button>
      </form>
      {p.problem && <p className="field-error small">{p.problem === "long" ? t("At most {max} characters.", { max: MAX_CHECKPOINT_NAME }) : t("Use lowercase letters, digits and hyphens, not at the ends.")}</p>}
      {p.note && (
        <p className="brow-note" role="status">
          {p.note}
        </p>
      )}
      {p.warning && (
        <p className="cps-warning small" role="status">
          {p.warning}
        </p>
      )}
      <ErrorLine error={p.error} />
      {graph && (rows.length > 0 || graph.lines.length > 1 || dots) && (
        <>
          {!modal && (
            <div className="hg-bar">
              <button type="button" className="hg-expand" ref={expandRef} aria-haspopup="dialog" title={t("Show the Checkpoints larger")} onClick={onExpand}>
                <Icon name="expand" />
                Expand
              </button>
            </div>
          )}
          <HistoryGraph
            branch={b.name}
            lines={graph.lines}
            checkpoints={graph.items}
            head={graph.head}
            events={p.events}
            fromTx={p.fromTx}
            recorded={b.record}
            scale={modal ? MODAL_SCALE : 1}
            selected={selected?.id ?? null}
            onSelect={p.select}
          />
          <p className="muted small hg-legend">{dots ? t("Dots are recorded transactions. Select a checkpoint or a dot.") : t("Select a checkpoint.")}</p>
          {selected && (
            <div className="hg-detail" role="region" aria-label="Selected" aria-live="polite">
              {selected.kind === "group" && selected.group ? (
                <GroupDetail
                  key={selected.id}
                  group={selected.group}
                  line={selected.shown}
                  asking={p.askingTx}
                  working={working}
                  why={p.txRestoreWhy}
                  branch={b.name}
                  onAsk={p.setAskingTx}
                  onRestore={(gtid) => p.restoreTx(selected.line, gtid)}
                  saveFirst={{ onSave: p.saveFirst, why: p.saveWhy }}
                />
              ) : selected.kind === "tx" && selected.tx ? (
                <TxDetail
                  tx={selected.tx}
                  line={selected.shown}
                  asking={p.askingTx}
                  working={working}
                  why={p.txRestoreWhy}
                  branch={b.name}
                  onAsk={p.setAskingTx}
                  onRestore={() => p.restoreTx(selected.line, selected.tx!.gtid)}
                  saveFirst={{ onSave: p.saveFirst, why: p.saveWhy }}
                />
              ) : selected.kind === "checkpoint" && selected.checkpoint ? (
                <CheckpointDetail
                  c={selected.checkpoint}
                  line={selected.shown}
                  branch={b.name}
                  asking={asking?.from === "graph" && asking.name === selected.checkpoint.name}
                  working={working}
                  why={p.restoreWhy}
                  onAsk={(on) => p.setAsking(on ? { what: "restore", name: selected.checkpoint!.name, from: "graph" } : null)}
                  onRestore={() => p.restoreCheckpoint(selected.checkpoint!.name)}
                  onFork={onFork ? () => onFork(selected.checkpoint!.name) : undefined}
                  forkWhy={p.forkWhy}
                  liveLine={graph.live}
                  mergeWhy={p.mergeWhy}
                  onMerge={() => openMerge(selected.checkpoint!.name, selected.checkpoint!.line)}
                  saveFirst={{ onSave: p.saveFirst, why: p.saveWhy }}
                />
              ) : (
                <p className="small">{t("The branch's creation state. Reset returns here.")}</p>
              )}
            </div>
          )}
          {!onMerge && localMerge && (
            <MergeBranchModal
              branch={b}
              checkpoint={localMerge.checkpoint}
              sourceLine={localMerge.line}
              onClose={() => setLocalMerge(null)}
            />
          )}
        </>
      )}
      {graph && rows.length === 0 && <p className="muted small">{t("No checkpoints yet.")}</p>}
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
                {c.runtime && <span className="muted small mono" title={t("The runtime the Branch ran when this Checkpoint was taken")}>{c.runtime}</span>}
                {p.showLines && <span className="muted small">line {p.shown(c.line)}</span>}
              </div>
              {asking?.name === c.name && asking.from === "list" ? (
                <div className="confirm" role="group" aria-label={asking.what === "restore" ? `Restore ${c.name}` : `Delete ${c.name}`} onKeyDown={(e) => e.key === "Escape" && (e.stopPropagation(), p.setAsking(null))}>
                  <span role="alert">
                    {asking.what === "restore"
                      ? t("Restore {name} to {checkpoint}? The current state of {name} is replaced and is not saved; changes after the last checkpoint are lost. Save a checkpoint first if you want to keep them. Every checkpoint you already have stays.", { name: b.name, checkpoint: c.name })
                      : t("Delete checkpoint {checkpoint}? The other checkpoints are not affected.", { checkpoint: c.name })}
                  </span>
                  <span className="confirm-buttons">
                    <button type="button" autoFocus onClick={() => p.setAsking(null)}>
                      {t("Cancel")}
                    </button>
                    {asking.what === "restore" && (
                      <button type="button" disabled={p.working || !!p.saveWhy} title={p.saveWhy} onClick={p.saveFirst}>
                        Save a checkpoint first
                      </button>
                    )}
                    <button
                      type="button"
                      className={asking.what === "delete" ? "danger-button" : "primary"}
                      disabled={working}
                      onClick={() => (asking.what === "restore" ? p.restoreCheckpoint(c.name) : p.deleteCheckpoint(c.name))}
                    >
                      {asking.what === "restore" ? "Confirm restore" : "Confirm delete"}
                    </button>
                  </span>
                </div>
              ) : (
                <span className="cp-actions">
                  <button type="button" disabled={!!p.restoreWhy || working} title={p.restoreWhy ?? t("Go back to this state")} onClick={() => p.setAsking({ what: "restore", name: c.name, from: "list" })}>
                    Restore
                  </button>
                  <button type="button" className="text-button danger" disabled={!!p.deleteWhy || working} title={p.deleteWhy} onClick={() => p.setAsking({ what: "delete", name: c.name, from: "list" })}>
                    Delete
                  </button>
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
    </>
  );
}

// The confirmation of a restore: what it does, in a sentence, before it is done.
function Confirm({ label, text, action, danger, working, onCancel, onConfirm, saveFirst }: { label: string; text: string; action: string; danger?: boolean; working: boolean; onCancel: () => void; onConfirm: () => void; saveFirst?: { onSave: () => void; why?: string } }) {
  return (
    <div className="confirm" role="group" aria-label={label} onKeyDown={(e) => e.key === "Escape" && (e.stopPropagation(), onCancel())}>
      <span role="alert">{text}</span>
      <span className="confirm-buttons">
        <button type="button" autoFocus onClick={onCancel}>
          {t("Cancel")}
        </button>
        {saveFirst && (
          <button type="button" disabled={working || !!saveFirst.why} title={saveFirst.why} onClick={saveFirst.onSave}>
            Save a checkpoint first
          </button>
        )}
        <button type="button" className={danger ? "danger-button" : "primary"} disabled={working} onClick={onConfirm}>
          {action}
        </button>
      </span>
    </div>
  );
}

function CheckpointDetail({ c, line, liveLine, branch, asking, working, why, forkWhy, mergeWhy, onAsk, onRestore, onFork, onMerge, saveFirst }: { c: import("../api").Checkpoint; line: number; liveLine: number; branch: string; asking: boolean; working: boolean; why?: string; forkWhy?: string; mergeWhy?: string; onAsk: (on: boolean) => void; onRestore: () => void; onFork?: () => void; onMerge?: () => void; saveFirst: { onSave: () => void; why?: string } }) {
  return (
    <div className="hg-detail-body">
      <div className="cp-main">
        <span className="mono strong">{c.name}</span>
        {notes(c).map((n) => (
          <span key={n} className={n === "current" ? "badge on" : "badge"}>
            {n}
          </span>
        ))}
        <span className="muted small">line {line}</span>
        <span className="muted small" title={new Date(c.created_at).toLocaleString()}>
          {relative(c.created_at)}
        </span>
        <span className="muted small">{bytes(c.size)}</span>
      </div>
      {asking ? (
        <Confirm
          label={`Restore ${c.name}`}
          text={t("Restore {name} to {checkpoint}? The current state of {name} is replaced and is not saved; changes after the last checkpoint are lost. Save a checkpoint first if you want to keep them. Every checkpoint you already have stays.", { name: branch, checkpoint: c.name })}
          action="Confirm restore"
          working={working}
          onCancel={() => onAsk(false)}
          onConfirm={onRestore}
          saveFirst={saveFirst}
        />
      ) : (
        <span className="cp-actions">
          <button type="button" disabled={!!why || working} title={why ?? t("Go back to this state")} onClick={() => onAsk(true)}>
            Restore
          </button>
          {onFork && (
            <button type="button" disabled={!!forkWhy || working} title={forkWhy} onClick={onFork}>
              Fork
            </button>
          )}
          {onMerge && (
            <button type="button" disabled={!!mergeWhy || c.line === liveLine || working} title={mergeWhy ?? (c.line === liveLine ? t("Select a checkpoint on another line to merge.") : t("Merge changes from this checkpoint's line into the live branch."))} onClick={onMerge}>
              Merge into live
            </button>
          )}
        </span>
      )}
    </div>
  );
}

// GroupDetail lists the transactions of a group in time order as a table; the
// chosen row (the last one at first) is shown below it with its restore.
function GroupDetail({ group, line, branch, asking, working, why, onAsk, onRestore, saveFirst }: { group: HistoryTx[]; line: number; branch: string; asking: boolean; working: boolean; why?: string; onAsk: (on: boolean) => void; onRestore: (gtid: string) => void; saveFirst: { onSave: () => void; why?: string } }) {
  const [pick, setPick] = useState(group[group.length - 1].gtid);
  const chosen = group.find((x) => x.gtid === pick) ?? group[group.length - 1];
  return (
    <div className="hg-detail-body">
      <div className="cp-main">
        <span className="strong">{group.length} transactions</span>
        <span className="muted small">line {line}</span>
        <span className="muted small" title={new Date(group[0].time).toLocaleString()}>
          {relative(group[group.length - 1].time)}
        </span>
      </div>
      <div className="hg-table-wrap">
        <table className="hg-table small">
          <thead>
            <tr>
              <th scope="col">#</th>
              <th scope="col">Time</th>
              <th scope="col">Rows</th>
              <th scope="col">Statement</th>
            </tr>
          </thead>
          <tbody>
            {group.map((x) => (
              <tr key={x.gtid} className={x.gtid === chosen.gtid ? "chosen" : ""}>
                <td>
                  <button
                    type="button"
                    className="link-button mono"
                    aria-pressed={x.gtid === chosen.gtid}
                    onClick={() => {
                      setPick(x.gtid);
                      onAsk(false);
                    }}
                  >
                    #{txNumber(x.gtid)}
                  </button>
                </td>
                <td className="mono muted" title={new Date(x.time).toLocaleString()}>
                  {new Date(x.time).toLocaleTimeString()}
                </td>
                <td className="mono">{rowsLabel(x)}</td>
                <td className="mono hg-stmt">{x.sql ? x.sql.replace(/\s+/g, " ").slice(0, 120) : ""}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <TxDetail brief tx={chosen} line={line} asking={asking} working={working} why={why} branch={branch} onAsk={onAsk} onRestore={() => onRestore(chosen.gtid)} saveFirst={saveFirst} />
    </div>
  );
}

function TxDetail({ tx, line, branch, asking, working, why, onAsk, onRestore, saveFirst, brief }: { brief?: boolean; tx: HistoryTx; line: number; branch: string; asking: boolean; working: boolean; why?: string; onAsk: (on: boolean) => void; onRestore: () => void; saveFirst: { onSave: () => void; why?: string } }) {
  return (
    <div className="hg-detail-body">
      {!brief && (
        <>
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
        </>
      )}
      {asking ? (
        <Confirm
          label="Restore to here"
          text={t("Restore {name} to just after this transaction? The branch continues on a new line. The current state of {name} is replaced and is not saved: save a checkpoint first if you may need it. Every checkpoint you already have stays.", { name: branch })}
          action="Confirm restore"
          working={working}
          onCancel={() => onAsk(false)}
          onConfirm={onRestore}
          saveFirst={saveFirst}
        />
      ) : (
        <button type="button" disabled={!!why || working} title={why ?? t("Go back to just after this transaction")} onClick={() => onAsk(true)}>
          {brief ? `Restore to #${txNumber(tx.gtid)}` : "Restore to here"}
        </button>
      )}
    </div>
  );
}
