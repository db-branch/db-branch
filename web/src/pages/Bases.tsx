import { FormEvent, useContext, useState } from "react";
import { api, Base, Branch, can, canBase, ownerLabel } from "../api";
import { BranchName } from "../components/branchname";
import { Engine, Icon, Status } from "../components/marks";
import { Dialog, Drawer, Empty, ErrorLine, ErrorText, Field, Info } from "../components/ui";
import { skippedReasons } from "../lib/bases";
import { FilterBar } from "../components/filterbar";
import { engineOf, filterBases, optionsOf } from "../lib/filter";
import { useFilter } from "../lib/usefilter";
import { GoContext } from "../lib/gocontext";
import { ADMIN } from "../lib/nav";
import { formatDateTime, relative } from "../lib/format";
import { LocalizedError, serverError } from "../lib/errors";
import { useLive, useNow } from "../lib/live";
import { t } from "../lib/i18n";
import { RefreshCard } from "./BaseRefresh";
import { AnonymousCard } from "./BaseAnonymous";
import { ReplicasCard } from "./BaseReplicas";
import { DumpPicker, DumpSource, dumpProblem, Progress, useDumpSender } from "../components/dumppicker";

export function Bases() {
  const go = useContext(GoContext);
  const { data: bases, error } = useLive(api.bases, ["base", "operation"]);
  const { data: branches } = useLive(api.branches, ["branch"]);
  const { data: cluster } = useLive(api.cluster, ["node"]);
  const [creating, setCreating] = useState(false);
  const [filter, setFilter] = useFilter(["q", "status", "engine", "node"]);
  useNow();

  const nodes = cluster?.mode === "cluster" ? cluster.nodes.map((n) => n.name) : [];
  const usedBy = (n: string) => (branches ?? []).filter((x) => x.base === n);
  const all = bases ?? [];
  const shown = filterBases(all, filter);
  const facets = [
    { key: "status", label: "Status", options: optionsOf(all.map((b) => b.status)) },
    { key: "engine", label: "Engine", options: optionsOf(all.map((b) => engineOf(b.runtime))) },
    { key: "node", label: "Node", options: nodes.length > 1 ? optionsOf(all.map((b) => b.node)) : [] },
  ];

  return (
    <section className="page">
      <header className="page-head">
        <h1>{t("Bases")}</h1>
        {can("base.create") && (
          <button className="primary" onClick={() => setCreating(true)}>
            <Icon name="plus" /> Base
          </button>
        )}
      </header>
      <ErrorLine error={error} />
      {bases && bases.length === 0 && <Empty line={t("No bases.")} action={can("base.create") ? <button onClick={() => setCreating(true)}>{t("Create a base")}</button> : undefined} />}
      {all.length > 0 && <FilterBar values={filter} onChange={setFilter} facets={facets} shown={shown.length} total={all.length} />}
      {all.length > 0 && shown.length === 0 && (
        <Empty
          line={t("No matching bases.")}
          action={
            <button type="button" onClick={() => setFilter({ q: "", status: "", engine: "", node: "" })}>
              {t("Clear filters")}
            </button>
          }
        />
      )}
      <div className="bases">
        {shown.map((b) => (
          <BaseLine key={b.name} base={b} branches={usedBy(b.name)} nodes={nodes} href={`${ADMIN}/bases/${encodeURIComponent(b.name)}`} onOpen={() => go(`${ADMIN}/bases/${encodeURIComponent(b.name)}`)} />
        ))}
      </div>
      {creating && <CreateBase onClose={() => setCreating(false)} />}
    </section>
  );
}

// A base is drawn as a timeline: snapshots are ticks on a line, and the
// branches cloned from each snapshot hang below it. Clicking it opens the
// detail panel, where it is managed.
function BaseLine({ base, branches, nodes, href, onOpen }: { base: Base; branches: Branch[]; nodes: string[]; href: string; onOpen: () => void }) {
  const clustered = nodes.length > 1;
  const bySnap = new Map<string, Branch[]>();
  for (const br of branches) bySnap.set(br.snapshot, [...(bySnap.get(br.snapshot) ?? []), br]);
  return (
    <article className="base clickable" onClick={onOpen}>
      <header className="base-head">
        <Status status={base.status} reason={base.status_reason} />
        <a
          className="base-name mono strong"
          href={href}
          aria-label={t("Open {name}", { name: base.name })}
          onClick={(e) => {
            e.stopPropagation();
            if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
            e.preventDefault();
            onOpen();
          }}
        >
          {base.name}
        </a>
        <Engine runtime={base.runtime} />
        {clustered && (
          <span className="mono muted" title={t("Node holding the base")}>
            {base.node}
          </span>
        )}
        {base.operation && <span className="badge">{base.operation.kind.replace("base.", "")}…</span>}
        {base.status_reason && <span className="reason"><ErrorText error={serverError(base.status_reason)} /></span>}
        <span className="spacer" />
        <span className="muted open-hint">{t("Details ›")}</span>
      </header>
      {base.description && <p className="base-desc">{base.description}</p>}
      <ol className="timeline">
        {base.snapshots.map((s) => {
          const kids = bySnap.get(s.name) ?? [];
          return (
            <li key={s.name} className={s.name === base.latest_snapshot ? "latest" : ""}>
              <span className="tick" title={formatDateTime(s.created_at)} />
              <span className="mono snap-name">
                {s.name}
                {s.public && <span className="badge on">public</span>}
              </span>
              <span className="muted snap-age">{relative(s.created_at)}</span>
              {clustered && <Holders nodes={nodes} holders={[base.node, ...(base.replicas[s.name] ?? [])]} skipped={skippedReasons(base.holders)} />}
              {kids.length > 0 && (
                <ul className="sprouts">
                  {kids.map((k) => (
                    <li key={k.name}>
                      <Status status={k.status} />
                      <span className="mono">{k.name}</span>
                    </li>
                  ))}
                </ul>
              )}
            </li>
          );
        })}
      </ol>
    </article>
  );
}

// One square per node, filled where the snapshot is held: branches from it
// can start on those nodes.
// A node the base is never copied to (other OS) is drawn greyed, with the reason on hover.
function Holders({ nodes, holders, skipped = {} }: { nodes: string[]; holders: string[]; skipped?: Record<string, string> }) {
  const held = new Set(holders);
  const off = nodes.filter((n) => !held.has(n) && n in skipped);
  const title = t("On {nodes}", { nodes: nodes.filter((n) => held.has(n)).join(", ") }) + off.map((n) => `\n${t("{node}: skipped ({reason})", { node: n, reason: skipped[n] })}`).join("");
  return (
    <span className="holders" title={title}>
      {nodes.map((n) => (
        <span key={n} className={held.has(n) ? "held" : n in skipped ? "skipped" : ""} />
      ))}
    </span>
  );
}

const publicHelp = () => (
  <>
    {t("Public: anyone can create a branch from this snapshot without logging in. Such branches are open to everyone, live for a limited time, and are capped in number per Base (Without sign-in) and cluster-wide (see Settings).")}
    <br />
    {t("Private (default): only logged-in users can branch from it.")}
    <br />
    {t("Neither changes the data or existing branches.")}
  </>
);

export function BaseBody({
  base,
  branches,
  nodes,
  onUpdate,
  onDelete,
}: {
  base: Base;
  branches: Branch[];
  nodes: string[];
  onUpdate: () => void;
  onDelete: () => void;
}) {
  const go = useContext(GoContext);
  const [error, setError] = useState<unknown>(null);
  const [confirming, setConfirming] = useState<string | null>(null);
  const run = (p: Promise<unknown>) => p.then(() => setError(null), (e: unknown) => setError(e));
  const ready = base.status === "ready" && !base.operation;
  const clustered = nodes.length > 1;
  const count = (snap: string) => branches.filter((b) => b.snapshot === snap).length;

  return (
    <>
      <div className="detail">
        <div className="detail-meta">
          <Status status={base.status} reason={base.status_reason} />
          <span>{base.operation ? `${base.operation.kind.replace("base.", "")}…` : base.status}</span>
          <Engine runtime={base.runtime} />
          {clustered && <span className="mono muted">{base.node}</span>}
          <span className="muted">{t("created {when}", { when: relative(base.created_at) })}</span>
        </div>
        {base.status_reason && <p className="reason"><ErrorText error={serverError(base.status_reason)} /></p>}

        <div className="detail-actions">
          {(canBase(base, "base.load") || canBase(base, "base.sql")) && (
          <button onClick={onUpdate} disabled={!ready} title={t("Load a dump or run SQL on the base server")}>
            {t("Update data")}
          </button>
          )}
          {canBase(base, "base.snapshot") && (
            <button onClick={() => run(api.snapshotBase(base.name))} disabled={!ready} title={t("Stops the base server cleanly, then snapshots")}>
              {t("Take snapshot")}
            </button>
          )}
          <span className="spacer" />
          {canBase(base, "base.delete") && (
            <button className="danger-button" onClick={onDelete} disabled={!!base.operation}>
              {t("Delete base…")}
            </button>
          )}
        </div>
        <ErrorLine error={error} />

        <Description base={base} />

        <ReplicasCard base={base} />

        <RefreshCard base={base} />

        <h3>
          Snapshots <span className="muted">{base.snapshots.length}</span>
        </h3>
        <div className="table-wrap">
          <table className="rows compact">
            <thead>
              <tr>
                <th>{t("Name")}</th>
                <th>{t("Taken")}</th>
                <th>{t("Branches")}</th>
                <th>
                  <span className="th-info">
                    Access <Info label={t("What public means")}>{publicHelp()}</Info>
                  </span>
                </th>
                {clustered && <th>{t("Nodes")}</th>}
                <th aria-label={t("Actions")} />
              </tr>
            </thead>
            <tbody>
              {[...base.snapshots].reverse().map((s) => {
                const latest = s.name === base.latest_snapshot;
                const used = count(s.name);
                return (
                  <tr key={s.name}>
                    <td>
                      <span className="mono">{s.name}</span>
                      {latest && <span className="badge">latest</span>}
                    </td>
                    <td className="muted" title={formatDateTime(s.created_at)}>
                      {relative(s.created_at)}
                    </td>
                    <td className="mono">{used}</td>
                    <td>
                      <div className="segmented small" role="radiogroup" aria-label={t("Access to {name}", { name: s.name })} aria-disabled={!canBase(base, "base.publish")}>
                        <button type="button" role="radio" aria-checked={!s.public} className={!s.public ? "on" : ""} disabled={!canBase(base, "base.publish")} onClick={() => s.public && run(api.setSnapshotPublic(base.name, s.name, false))}>
                          {t("Private")}
                        </button>
                        <button type="button" role="radio" aria-checked={s.public} className={s.public ? "on" : ""} disabled={!canBase(base, "base.publish")} onClick={() => !s.public && run(api.setSnapshotPublic(base.name, s.name, true))}>
                          {t("Public")}
                        </button>
                      </div>
                    </td>
                    {clustered && (
                      <td>
                        <Holders nodes={nodes} holders={[base.node, ...(base.replicas[s.name] ?? [])]} skipped={skippedReasons(base.holders)} />
                      </td>
                    )}
                    <td className="col-actions">
                      {latest || used > 0 ? (
                        <span className="muted small" title={latest ? t("The latest snapshot is kept") : t("Branches use it")}>
                          {latest ? "kept" : "in use"}
                        </span>
                      ) : !(canBase(base, "base.snapshot.delete") || canBase(base, "base.snapshot_delete")) ? null : (
                        <button
                          type="button"
                          className="text-button danger"
                          onClick={() => (confirming === s.name ? (setConfirming(null), run(api.deleteSnapshot(base.name, s.name))) : setConfirming(s.name))}
                          onBlur={() => setConfirming(null)}
                        >
                          {confirming === s.name ? t("Really delete?") : t("Delete")}
                        </button>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
        <p className="muted small">{t("Older snapshots no branch uses are deleted automatically once a newer one exists (Settings → Prune old snapshots).")}</p>

        <AnonymousCard base={base} branches={branches} />

        <h3>
          Branches <span className="muted">{branches.length}</span>
        </h3>
        {branches.length === 0 ? (
          <p className="muted">{t("None. The base can be deleted.")}</p>
        ) : (
          <ul className="plain-list">
            {branches.map((b) => (
              <li key={b.name}>
                <Status status={b.status} />
                <a
                  className="branch-link"
                  href={`${ADMIN}/branches/${encodeURIComponent(b.name)}`}
                  onClick={(e) => {
                    if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
                    e.preventDefault();
                    go(`${ADMIN}/branches/${encodeURIComponent(b.name)}`);
                  }}
                >
                  <BranchName name={b.name} owner={ownerLabel(b)} />
                </a>
                <span className="muted mono">@{b.snapshot}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
    </>
  );
}

function Description({ base }: { base: Base }) {
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState(base.description);
  const [error, setError] = useState<unknown>(null);
  const save = async () => {
    try {
      await api.describeBase(base.name, text);
      setEditing(false);
      setError(null);
    } catch (e) {
      setError(e);
    }
  };
  if (!editing) {
    return (
      <div className="description">
        {base.description ? <p>{base.description}</p> : <p className="muted">{t("No description.")}</p>}
        {canBase(base, "base.describe") && (
          <button
            className="text-button"
            onClick={() => {
              setText(base.description);
              setEditing(true);
            }}
          >
            {base.description ? t("Edit description") : "+ Add description"}
          </button>
        )}
      </div>
    );
  }
  return (
    <div className="description">
      <textarea rows={3} value={text} maxLength={1000} onChange={(e) => setText(e.target.value)} placeholder={t("What this base holds, e.g. masked production data, refreshed every Monday")} autoFocus />
      <ErrorLine error={error} />
      <div className="form-actions">
        <button onClick={() => setEditing(false)}>{t("Cancel")}</button>
        <button className="primary" onClick={save}>
          {t("Save")}
        </button>
      </div>
    </div>
  );
}


function CreateBase({ onClose }: { onClose: () => void }) {
  const [name, setName] = useState("");
  const [runtime, setRuntime] = useState("");
  const [description, setDescription] = useState("");
  const [dump, setDump] = useState<DumpSource>({ kind: "none" });
  const { data: cluster } = useLive(api.cluster, ["node"]);
  const { data: caps } = useLive(api.capabilities, []);
  const [node, setNode] = useState("");
  const target = node || caps?.node || "";
  // Engines installed on the node that will hold the base.
  const runtimes = cluster?.nodes.find((n) => n.name === target)?.runtimes.map((runtime) => ({ runtime })) ?? cluster?.nodes[0]?.runtimes.map((runtime) => ({ runtime }));
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const chosen = runtimes?.some((r) => r.runtime === runtime) ? runtime : (runtimes?.[0]?.runtime ?? "");
  const sender = useDumpSender();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      const ref = await sender.send(dump, target || undefined);
      await api.createBase({ name, runtime: chosen, dump: ref, node: node || undefined, description: description.trim() || undefined });
      onClose();
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };
  return (
    <Drawer title={t("New base")} onClose={onClose}>
      <form className="form" onSubmit={submit}>
        <Field label={t("Name")}>
          <input className="mono" value={name} onChange={(e) => setName(e.target.value)} placeholder="prod-masked" autoFocus required pattern="[a-z]([a-z0-9]|-[a-z0-9])*" maxLength={32} />
        </Field>
        <Field label={t("Description")} hint={t("Optional. Helps people pick the right base.")}>
          <textarea rows={2} value={description} maxLength={1000} onChange={(e) => setDescription(e.target.value)} placeholder={t("Masked production data, refreshed every Monday")} />
        </Field>
        <Field label={t("Engine")} hint={runtimes?.length === 0 ? t("Install one on this node under Cluster") : undefined}>
          <select value={chosen} onChange={(e) => setRuntime(e.target.value)}>
            {runtimes?.map((r) => (
              <option key={r.runtime} value={r.runtime}>
                {r.runtime}
              </option>
            ))}
          </select>
        </Field>
        {cluster?.mode === "cluster" && (
          <Field label={t("Node")} hint={t("Holds the base; its branches start there.")}>
            <select className="mono" value={target} onChange={(e) => setNode(e.target.value)}>
              {cluster.nodes.map((n) => (
                <option key={n.name} value={n.name} disabled={n.down}>
                  {n.name}
                </option>
              ))}
            </select>
          </Field>
        )}
        <DumpPicker value={dump} onChange={setDump} allowNone node={target || undefined} />
        {sender.progress !== null && <Progress value={sender.progress} onCancel={sender.cancel} />}
        <ErrorLine error={error} />
        <div className="form-actions">
          <button className="primary" disabled={busy || !name || !chosen || dumpProblem(dump, null) !== null}>
            {sender.progress !== null ? t("Uploading…") : t("Create")}
          </button>
        </div>
      </form>
    </Drawer>
  );
}

// UpdateData changes the base itself: load a dump, or run SQL. Then (by
// default) it snapshots, so new branches see the change. Existing branches
// keep their snapshot.
export function UpdateData({ base, onClose }: { base: Base; onClose: () => void }) {
  const [mode, setMode] = useState<"dump" | "sql">("dump");
  const [sql, setSql] = useState("");
  const [dump, setDump] = useState<DumpSource>({ kind: "file", file: null });
  const [snapshot, setSnapshot] = useState(true);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const sender = useDumpSender();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      if (mode === "sql") {
        await api.baseSQL(base.name, sql);
        if (snapshot) await api.snapshotBase(base.name);
      } else {
        const ref = await sender.send(dump, base.node);
        if (!ref) throw new LocalizedError(t("Choose a dump file or a path."));
        await api.loadBase(base.name, ref, snapshot);
      }
      onClose();
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };
  const ok = mode === "sql" ? sql.trim() !== "" : dumpProblem(dump, null) === null;
  return (
    <Drawer title={t("Update {name}", { name: base.name })} onClose={onClose}>
      <form className="form" onSubmit={submit}>
        <div className="segmented" role="radiogroup" aria-label={t("How to update")}>
          <button type="button" role="radio" aria-checked={mode === "dump"} className={mode === "dump" ? "on" : ""} onClick={() => setMode("dump")}>
            {t("Load a dump")}
          </button>
          <button type="button" role="radio" aria-checked={mode === "sql"} className={mode === "sql" ? "on" : ""} onClick={() => setMode("sql")}>
            {t("Run SQL")}
          </button>
        </div>
        {mode === "dump" ? (
          <>
            <DumpPicker value={dump} onChange={setDump} allowNone={false} node={base.node} />
            <p className="muted small">{t("The dump runs on top of the current data, as the administrator. A dump with DROP/CREATE statements replaces what it covers.")}</p>
          </>
        ) : (
          <Field label={t("SQL")} hint={t("Runs as the administrator on the base server.")}>
            <textarea className="mono" rows={10} value={sql} onChange={(e) => setSql(e.target.value)} placeholder={t("INSERT INTO shop.products (name) VALUES ('new item');")} autoFocus />
          </Field>
        )}
        <label className="check">
          <input type="checkbox" checked={snapshot} onChange={(e) => setSnapshot(e.target.checked)} /> {t("Take a snapshot afterwards (new branches use it)")}
        </label>
        <p className="muted small">{t("Existing branches are not affected.")}</p>
        {sender.progress !== null && <Progress value={sender.progress} onCancel={sender.cancel} />}
        <ErrorLine error={error} />
        <div className="form-actions">
          <button className="primary" disabled={busy || !ok}>
            {sender.progress !== null ? t("Uploading…") : mode === "dump" ? t("Load") : t("Run")}
          </button>
        </div>
      </form>
    </Drawer>
  );
}

// DeleteBase asks for the base's name before deleting it with every
// snapshot. A base with branches cannot be deleted.
export function DeleteBase({ base, branches, onClose, onDeleted }: { base: Base; branches: number; onClose: () => void; onDeleted: () => void }) {
  const [typed, setTyped] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const del = () => {
    setBusy(true);
    api.deleteBase(base.name).then(onDeleted, (e: unknown) => {
      setError(e);
      setBusy(false);
    });
  };
  return (
    <Dialog title={t("Delete base {name}?", { name: base.name })} onClose={onClose}>
      {branches > 0 ? (
        <p className="muted">{t("{count} branch(es) still use this base. Delete them first.", { count: branches })}</p>
      ) : (
        <>
          <p>{t("Are you sure? The base server, its data and all {count} snapshot(s) are deleted on every node. This cannot be undone.", { count: base.snapshots.length })}</p>
          <Field label={t("Type the base name to confirm")}>
            <input className="mono" value={typed} onChange={(e) => setTyped(e.target.value)} autoFocus placeholder={base.name} />
          </Field>
        </>
      )}
      <ErrorLine error={error} />
      <div className="form-actions">
        <button onClick={onClose}>{t("Cancel")}</button>
        <button className="danger-button" disabled={busy || branches > 0 || typed !== base.name} onClick={del}>
          {t("Delete base")}
        </button>
      </div>
    </Dialog>
  );
}
