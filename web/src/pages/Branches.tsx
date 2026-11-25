import { FormEvent, useContext, useState } from "react";
import { api, Base, Branch, can, isAdmin, ownerLabel } from "../api";
import { Icon, Status } from "../components/marks";
import { Dialog, Drawer, Empty, ErrorLine, ErrorText, Field } from "../components/ui";
import { serverError } from "../lib/errors";
import { relative } from "../lib/format";
import { useLive, useNow } from "../lib/live";
import { t } from "../lib/i18n";
import { RecordingBadge } from "../components/recording";
import { BranchName } from "../components/branchname";
import { FilterBar } from "../components/filterbar";
import { engineOf, filterBranches, optionsOf } from "../lib/filter";
import { useFilter } from "../lib/usefilter";
import { GoContext } from "../lib/gocontext";
import { ADMIN } from "../lib/nav";

export function Branches() {
  const go = useContext(GoContext);
  const { data: branches, error } = useLive(api.branches, ["branch", "operation"]);
  const { data: bases } = useLive(api.bases, ["base"]);
  const [creating, setCreating] = useState(false);
  const [filter, setFilter] = useFilter(["q", "status", "engine", "base", "owner"]);
  useNow();
  const ready = (bases ?? []).filter((b) => b.latest_snapshot);
  const all = branches ?? [];
  const shown = filterBranches(all, filter);
  const facets = [
    { key: "status", label: "Status", options: optionsOf(all.map((b) => b.status)) },
    { key: "engine", label: "Engine", options: optionsOf(all.map((b) => engineOf(b.runtime))) },
    { key: "base", label: "Base", options: optionsOf(all.map((b) => b.base)) },
    { key: "owner", label: "Owner", options: optionsOf(all.map((b) => ownerLabel(b))) },
  ];

  return (
    <section className="page">
      <header className="page-head">
        <h1>{t("Branches")}</h1>
        {can("branch.create") && (
          <button className="primary" onClick={() => setCreating(true)} disabled={ready.length === 0} title={ready.length === 0 ? t("Create a base first") : undefined}>
            <Icon name="plus" /> Branch
          </button>
        )}
      </header>
      <ErrorLine error={error} />
      {branches && branches.length === 0 && (
        <Empty
          line={ready.length === 0 ? t("No bases yet.") : t("No branches.")}
          action={ready.length > 0 && can("branch.create") && <button onClick={() => setCreating(true)}>{t("Create a branch")}</button>}
        />
      )}
      {branches && branches.length > 0 && <FilterBar values={filter} onChange={setFilter} facets={facets} shown={shown.length} total={all.length} />}
      {branches && branches.length > 0 && shown.length === 0 && (
        <Empty
          line={t("No matching branches.")}
          action={
            <button type="button" onClick={() => setFilter({ q: "", status: "", engine: "", base: "", owner: "" })}>
              {t("Clear filters")}
            </button>
          }
        />
      )}
      {branches && shown.length > 0 && (
        <div className="table-wrap">
          <table className="rows">
            <thead>
              <tr>
                <th className="col-status" aria-label={t("Status")} />
                <th>{t("Name")}</th>
                <th>{t("Base")}</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((b) => (
                <tr key={b.name} className={b.operation ? "busy" : ""}>
                  <td className="col-status">
                    <Status status={b.status} reason={b.status_reason} />
                  </td>
                  <td>
                    <a className="branch-link" href={`${ADMIN}/branches/${encodeURIComponent(b.name)}`} onClick={(e) => {
                      if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
                      e.preventDefault();
                      go(`${ADMIN}/branches/${encodeURIComponent(b.name)}`);
                    }}>
                      <BranchName name={b.name} owner={ownerLabel(b)} />
                    </a>
                    {b.access === "open" && (
                      <span className="badge" title={t("Anyone may connect, with the shared password")}>
                        open
                      </span>
                    )}
                    <RecordingBadge record={b.record} />
                    {b.status_reason && <span className="reason"><ErrorText error={serverError(b.status_reason)} /></span>}
                  </td>
                  <td>
                    <span className="mono muted" title={`snapshot ${b.snapshot}`}>
                      {b.base}
                    </span>
                    {b.parent_branch && (
                      <span className="muted small mono" title={t("Forked from {name}", { name: b.parent_branch })}>
                        {" "}
                        ← {b.parent_branch}
                      </span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {creating && <CreateBranch bases={ready} onClose={() => setCreating(false)} />}
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
  const [error, setError] = useState<unknown>(null);
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
      setError(err);
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
          <Field label={t("Custom TTL")} hint={custom && !customTTL ? t("Use e.g. 30m, 2h, 1h30m or 14d") : t("e.g. 30m, 2h, 1h30m or 14d")}>
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

export function DeleteBranch({ branch, onClose, onDeleted }: { branch: Branch; onClose: () => void; onDeleted?: () => void }) {
  const [typed, setTyped] = useState("");
  const [error, setError] = useState<unknown>(null);
  return (
    <Dialog title={t("Delete {name}?", { name: branch.name })} onClose={onClose}>
      <p className="muted">{t("Data in this branch is lost. Created {when}.", { when: relative(branch.created_at) })}</p>
      <Field label={t("Type the branch name")}>
        <input className="mono" value={typed} placeholder={branch.name} onChange={(e) => setTyped(e.target.value)} autoFocus />
      </Field>
      <ErrorLine error={error} />
      <div className="form-actions">
        <button onClick={onClose}>{t("Cancel")}</button>
        <button
          className="danger-button"
          disabled={typed !== branch.name}
          onClick={() => api.deleteBranch(branch.name).then(() => (onDeleted ? onDeleted() : onClose()), (e: unknown) => setError(e))}
        >
          {t("Delete")}
        </button>
      </div>
    </Dialog>
  );
}
