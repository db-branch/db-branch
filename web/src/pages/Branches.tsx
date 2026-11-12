import { FormEvent, useState } from "react";
import { api, Base, Branch, can, isAdmin } from "../api";
import { CheckpointPanel } from "../components/checkpoints";
import { showCheckpoints } from "../lib/checkpoints";
import { ConnectPanel } from "../components/connect";
import { CopyButton, Engine, Icon, Status, StorageBar, TtlBar } from "../components/marks";
import { Dialog, Drawer, Empty, ErrorLine, Field, Menu } from "../components/ui";
import { relative } from "../lib/format";
import { useLive, useNow } from "../lib/live";
import { splitOwned } from "../lib/branchname";
import { t } from "../lib/i18n";
import { RecordingBadge } from "../components/recording";

export function Branches() {
  const { data: branches, error } = useLive(api.branches, ["branch", "operation"]);
  const { data: bases } = useLive(api.bases, ["base"]);
  const { data: cluster } = useLive(api.cluster, ["node"]);
  const clustered = cluster?.mode === "cluster";
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<Branch | null>(null);
  const [connecting, setConnecting] = useState<Branch | null>(null);
  const [checkpointing, setCheckpointing] = useState<Branch | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  useNow();

  const run = (p: Promise<unknown>) => p.then(() => setActionError(null), (e: Error) => setActionError(e.message));
  const max = Math.max(1, ...(branches ?? []).map((b) => b.storage?.referenced ?? 0));
  const ready = (bases ?? []).filter((b) => b.latest_snapshot);

  return (
    <section className="page">
      <header className="page-head">
        <h1>{t("Branches")}</h1>
        <button className="primary" onClick={() => setCreating(true)} disabled={ready.length === 0} title={ready.length === 0 ? t("Create a base first") : undefined}>
          <Icon name="plus" /> Branch
        </button>
      </header>
      <ErrorLine error={error ?? actionError} />
      {branches && branches.length === 0 && (
        <Empty
          line={ready.length === 0 ? t("No bases yet.") : t("No branches.")}
          action={ready.length > 0 && <button onClick={() => setCreating(true)}>{t("Create a branch")}</button>}
        />
      )}
      {branches && branches.length > 0 && (
        <div className="table-wrap">
        <table className="rows">
          <thead>
            <tr>
              <th className="col-status" aria-label={t("Status")} />
              <th>{t("Name")}</th>
              <th>{t("Base")}</th>
              <th>{t("Engine")}</th>
              <th>{t("Endpoint")}</th>
              <th>{t("TTL")}</th>
              <th>{t("Storage")}</th>
              <th aria-label={t("Actions")} />
            </tr>
          </thead>
          <tbody>
            {branches.map((b) => (
              <tr key={b.name} className={b.operation ? "busy" : ""}>
                <td className="col-status">
                  <Status status={b.status} reason={b.status_reason} />
                </td>
                <td>
                  <BranchName name={b.name} owner={b.owner} />
                  {b.access === "open" && (
                    <span className="badge" title={t("Anyone may connect, with the shared password")}>
                      open
                    </span>
                  )}
                  <RecordingBadge record={b.record} />
                  {b.status_reason && <span className="reason">{b.status_reason}</span>}
                </td>
                <td>
                  <span className="mono muted" title={`snapshot ${b.snapshot}`}>
                    {b.base}
                  </span>
                </td>
                <td>
                  <Engine runtime={b.runtime} />
                </td>
                <td>
                  <span className="endpoint" title={clustered ? b.node : undefined}>
                    {clustered && <span className="mono muted">{b.connection.host}:</span>}
                    <span className="mono">{b.connection.port}</span>
                    {b.status === "ready" && <CopyButton label={t("Copy connection URL")} load={() => api.dsn(b.name)} />}
                  </span>
                </td>
                <td>
                  <TtlBar created={b.created_at} expires={b.expires_at} />
                </td>
                <td>
                  <StorageBar storage={b.storage} max={max} />
                </td>
                <td className="col-actions">
                  <Menu
                    label={`Actions for ${b.name}`}
                    items={[
                      ...(b.status === "ready" ? [{ label: t("Connect…"), onSelect: () => setConnecting(b) }] : []),
                      ...(["ready", "restoring"].includes(b.status) && showCheckpoints(b, can) ? [{ label: t("Checkpoints…"), onSelect: () => setCheckpointing(b) }] : []),
                      ...(b.status === "lost" ? [{ label: t("Recreate"), onSelect: () => run(api.recreateBranch(b.name)) }] : []),
                      ...(b.proxy
                        ? [{ label: t("Copy proxy URL"), onSelect: () => run(api.dsn(b.name, "proxy").then((d) => navigator.clipboard.writeText(d))) }]
                        : []),
                      { label: t("Reset"), onSelect: () => run(api.resetBranch(b.name)), disabled: b.status !== "ready" },
                      { label: t("Extend 1h"), onSelect: () => run(api.extendBranch(b.name, "1h")) },
                      { label: t("Extend 24h"), onSelect: () => run(api.extendBranch(b.name, "24h")) },
                      { label: t("Extend 7d"), onSelect: () => run(api.extendBranch(b.name, "168h")) },
                      { label: t("Delete…"), onSelect: () => setDeleting(b), danger: true, disabled: !["ready", "failed", "lost"].includes(b.status) },
                    ]}
                  />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        </div>
      )}
      {creating && <CreateBranch bases={ready} onClose={() => setCreating(false)} />}
      {deleting && <DeleteBranch branch={deleting} onClose={() => setDeleting(null)} />}
      {checkpointing && (
        <Dialog title={`Checkpoints of ${checkpointing.id || checkpointing.name}`} onClose={() => setCheckpointing(null)}>
          <CheckpointPanel branch={checkpointing} ready={branches?.find((x) => x.name === checkpointing.name)?.status === "ready"} allow={can} />
          <div className="form-actions">
            <button onClick={() => setCheckpointing(null)}>{t("Close")}</button>
          </div>
        </Dialog>
      )}
      {connecting && (
        <Dialog title={`Connect to ${connecting.id || connecting.name}`} onClose={() => setConnecting(null)}>
          <ConnectPanel branch={connecting.name} />
          <div className="form-actions">
            <button onClick={() => setConnecting(null)}>{t("Close")}</button>
          </div>
        </Dialog>
      )}
    </section>
  );
}

const ttls: [string, string][] = [
  ["1h", "1h"],
  ["4h", "4h"],
  ["24h", "1 day"],
  ["72h", "3 days"],
  ["168h", "7 days"],
  ["", "None"],
];

// parseTTL accepts what Go's time.ParseDuration does, plus days ("2d").
function parseTTL(v: string): string | null {
  const t = v.trim().toLowerCase();
  const d = t.match(/^(\d+)\s*d$/);
  if (d) return `${Number(d[1]) * 24}h`;
  return /^(\d+(\.\d+)?(h|m|s))+$/.test(t) && !/^0+(h|m|s)$/.test(t) ? t : null;
}

function CreateBranch({ bases, onClose }: { bases: Base[]; onClose: () => void }) {
  const [name, setName] = useState("");
  const [base, setBase] = useState(bases[0]?.name ?? "");
  const [ttl, setTtl] = useState("72h");
  const [custom, setCustom] = useState<string | null>(null);
  const customTTL = custom === null ? null : parseTTL(custom);
  const [sql, setSql] = useState("");
  const [showSql, setShowSql] = useState(false);
  const [owner, setOwner] = useState("");
  const [access, setAccess] = useState<"private" | "open">("private");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const snap = bases.find((b) => b.name === base)?.latest_snapshot;
  const admin = isAdmin();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await api.createBranch({ name, base, ttl: (custom !== null ? customTTL : ttl) || undefined, init_sql: sql || undefined, owner: owner.trim() || undefined, access });
      onClose();
    } catch (err) {
      setError((err as Error).message);
      setBusy(false);
    }
  };
  return (
    <Drawer title={t("New branch")} onClose={onClose}>
      <form className="form" onSubmit={submit}>
        <Field label={t("Name")}>
          <input className="mono" value={name} onChange={(e) => setName(e.target.value)} placeholder="feature-x" autoFocus required pattern="[a-z]([a-z0-9]|-[a-z0-9])*" maxLength={32} />
        </Field>
        <Field label={t("Base")} hint={snap ? `snapshot ${snap}` : undefined}>
          <select value={base} onChange={(e) => setBase(e.target.value)}>
            {bases.map((b) => (
              <option key={b.name} value={b.name}>
                {b.name}
              </option>
            ))}
          </select>
        </Field>
        <Field label={t("TTL")} hint={t("Deleted automatically after this long. It can be extended later.")}>
          <div className="segmented" role="radiogroup">
            {ttls.map(([v, l]) => (
              <button
                type="button"
                key={v}
                role="radio"
                aria-checked={custom === null && ttl === v}
                className={custom === null && ttl === v ? "on" : ""}
                onClick={() => {
                  setTtl(v);
                  setCustom(null);
                }}
              >
                {l}
              </button>
            ))}
            <button type="button" role="radio" aria-checked={custom !== null} className={custom !== null ? "on" : ""} onClick={() => setCustom(custom ?? "")}>
              {t("Other")}
            </button>
          </div>
        </Field>
        {custom !== null && (
          <Field label={t("Custom TTL")} hint={custom && !customTTL ? t("Use e.g. 30m, 2h, 1h30m or 14d") : "e.g. 30m, 2h, 1h30m or 14d"}>
            <input className="mono" value={custom} onChange={(e) => setCustom(e.target.value)} placeholder="30m" autoFocus />
          </Field>
        )}
        <Field
          label={t("Access")}
          hint={access === "private" ? t("A new random password. Only the owner and admins may connect.") : t("The cluster's shared password. Anyone may connect and query.")}
        >
          <div className="segmented" role="radiogroup">
            {(["private", "open"] as const).map((v) => (
              <button type="button" key={v} role="radio" aria-checked={access === v} className={access === v ? "on" : ""} onClick={() => setAccess(v)}>
                {v === "private" ? t("Private") : t("Anyone")}
              </button>
            ))}
          </div>
        </Field>
        {admin && (
          <Field label={t("Owner")} hint={t("Who manages it. Empty: you.")}>
            <input className="mono" value={owner} onChange={(e) => setOwner(e.target.value)} placeholder="alice" maxLength={64} />
          </Field>
        )}
        {showSql ? (
          <Field label={t("Init SQL")} hint={t("Runs once after cloning. Reset returns here.")}>
            <textarea className="mono" rows={8} value={sql} onChange={(e) => setSql(e.target.value)} placeholder={t("UPDATE users SET email = CONCAT('user', id, '@example.invalid');")} />
          </Field>
        ) : (
          <button type="button" className="text-button" onClick={() => setShowSql(true)}>
            {t("+ Init SQL")}
          </button>
        )}
        <ErrorLine error={error} />
        <div className="form-actions">
          <button className="primary" disabled={busy || !name || !base || (custom !== null && !customTTL)}>
            {t("Create")}
          </button>
        </div>
      </form>
    </Drawer>
  );
}

function DeleteBranch({ branch, onClose }: { branch: Branch; onClose: () => void }) {
  const [typed, setTyped] = useState("");
  const [error, setError] = useState<string | null>(null);
  return (
    <Dialog title={`Delete ${branch.name}`} onClose={onClose}>
      <p className="muted">Data in this branch is lost. Created {relative(branch.created_at)}.</p>
      <Field label={t("Type the branch name")}>
        <input className="mono" value={typed} onChange={(e) => setTyped(e.target.value)} autoFocus />
      </Field>
      <ErrorLine error={error} />
      <div className="form-actions">
        <button onClick={onClose}>{t("Cancel")}</button>
        <button
          className="danger-button"
          disabled={typed !== branch.name}
          onClick={() => api.deleteBranch(branch.name).then(onClose, (e: Error) => setError(e.message))}
        >
          {t("Delete")}
        </button>
      </div>
    </Dialog>
  );
}

// BranchName shows a branch name. A name that starts with its owner's prefix
// has that prefix muted; otherwise the owner is shown muted before the name.
function BranchName({ name, owner }: { name: string; owner?: string | null }) {
  const [prefix, rest] = splitOwned(name, owner ?? undefined);
  if (prefix) {
    return (
      <span className="mono strong" title={owner ?? undefined}>
        <span className="muted">{prefix}</span>
        {rest}
      </span>
    );
  }
  return (
    <>
      {owner && (
        <span className="mono muted" title={owner}>
          {owner.split("@")[0]}/
        </span>
      )}
      <span className="mono strong">{name}</span>
    </>
  );
}
