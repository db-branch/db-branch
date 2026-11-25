import { FormEvent, useState } from "react";
import { api, Base, BaseRefresh, canBase } from "../api";
import { Dialog, ErrorLine, ErrorText, Field } from "../components/ui";
import { serverError } from "../lib/errors";
import { formatDateTime, relative } from "../lib/format";
import { useLive, useNow } from "../lib/live";
import { t } from "../lib/i18n";

// RefreshCard shows the base's scheduled refresh from a source database:
// the source (never its password), interval, last run and next run.
export function RefreshCard({ base }: { base: Base }) {
  const { data: refresh, error: loadError, reload } = useLive(() => api.baseRefresh(base.name), ["base", "operation"]);
  const [editing, setEditing] = useState(false);
  const [confirmingOff, setConfirmingOff] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const now = useNow();
  const manage = canBase(base, "base.refresh");
  const act = (p: Promise<unknown>) => p.then(() => (setError(null), reload()), (e: unknown) => setError(e));
  const refreshing = base.operation?.kind === "base.refresh";
  const idle = base.status === "ready" && !base.operation;

  if (refresh === undefined) return loadError ? <ErrorLine error={loadError} /> : null;
  if (refresh === null && !manage) return null;
  return (
    <section className="panel refresh-card" aria-label={t("Scheduled refresh")}>
      <h2>{t("Scheduled refresh")}</h2>
      {refresh === null ? (
        <>
          <p className="muted">{t("Not scheduled. The base is refreshed by hand.")}</p>
          <div className="form-actions">
            <button onClick={() => setEditing(true)}>{t("Set up…")}</button>
          </div>
        </>
      ) : (
        <>
          <dl className="kv">
            <dt>{t("Source")}</dt>
            <dd className="mono">
              {refresh.source.user}@{refresh.source.host}:{refresh.source.port} ({refresh.source.password_set ? t("password set") : t("no password")})
            </dd>
            <dt>{t("Databases")}</dt>
            <dd className="mono">{refresh.source.databases.join(", ")}</dd>
            <dt>{t("Every")}</dt>
            <dd className="mono">{refresh.every}</dd>
            <dt>{t("Last run")}</dt>
            <dd>
              {refreshing ? (
                t("Running…")
              ) : refresh.last_run ? (
                <>
                  <span className={refresh.last_run.ok ? "" : "error-text"}>{refresh.last_run.ok ? t("Succeeded") : t("Failed")}</span>{" "}
                  <span className="muted" title={formatDateTime(refresh.last_run.finished_at)}>
                    {relative(refresh.last_run.finished_at, now)}
                  </span>
                  {refresh.last_run.error && <p className="reason"><ErrorText error={serverError(refresh.last_run.error)} /></p>}
                </>
              ) : (
                <span className="muted">{t("Not yet")}</span>
              )}
            </dd>
            <dt>{t("Next run")}</dt>
            <dd title={formatDateTime(refresh.next_run)}>{relative(refresh.next_run, now)}</dd>
          </dl>
          <ErrorLine error={error} />
          {manage && (
            <div className="form-actions">
              <button onClick={() => act(api.runBaseRefresh(base.name))} disabled={!idle} title={t("Dump the source, load it into the base and take a snapshot now")}>
                {t("Run now")}
              </button>
              <button onClick={() => setEditing(true)}>{t("Edit")}</button>
              <button
                className="danger-button"
                onClick={() => (confirmingOff ? (setConfirmingOff(false), act(api.clearBaseRefresh(base.name))) : setConfirmingOff(true))}
                onBlur={() => setConfirmingOff(false)}
              >
                {confirmingOff ? t("Really turn off?") : t("Turn off")}
              </button>
            </div>
          )}
        </>
      )}
      {editing && (
        <EditRefresh
          base={base}
          current={refresh}
          onClose={() => setEditing(false)}
          onSaved={() => {
            setEditing(false);
            reload();
          }}
        />
      )}
    </section>
  );
}

const defaultPort = (runtime: string) => (runtime.startsWith("postgres") ? 5432 : 3306);

function EditRefresh({ base, current, onClose, onSaved }: { base: Base; current: BaseRefresh | null; onClose: () => void; onSaved: () => void }) {
  const [host, setHost] = useState(current?.source.host ?? "");
  const [port, setPort] = useState(String(current?.source.port ?? defaultPort(base.runtime)));
  const [user, setUser] = useState(current?.source.user ?? "");
  const [password, setPassword] = useState("");
  const [databases, setDatabases] = useState(current?.source.databases.join(", ") ?? "");
  const [every, setEvery] = useState(current?.every.replace(/h0m0s$/, "h").replace(/m0s$/, "m") ?? "24h");
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const dbs = databases.split(",").map((d) => d.trim()).filter(Boolean);
  const ok = host.trim() !== "" && Number(port) > 0 && user.trim() !== "" && dbs.length > 0 && every.trim() !== "";

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await api.setBaseRefresh(base.name, { source: { host: host.trim(), port: Number(port), user: user.trim(), password, databases: dbs }, every: every.trim() });
      onSaved();
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };
  return (
    <Dialog title={t("Scheduled refresh of {name}", { name: base.name })} onClose={onClose}>
      <form className="form" onSubmit={submit}>
        <Field label={t("Source host")}>
          <input className="mono" value={host} onChange={(e) => setHost(e.target.value)} autoFocus placeholder="db.internal" />
        </Field>
        <Field label={t("Port")}>
          <input className="mono" inputMode="numeric" value={port} onChange={(e) => setPort(e.target.value)} />
        </Field>
        <Field label={t("User")} hint={t("Read access is enough.")}>
          <input className="mono" value={user} onChange={(e) => setUser(e.target.value)} autoComplete="off" />
        </Field>
        <Field label={t("Password")} hint={current?.source.password_set ? t("Leave empty to keep the stored password.") : undefined}>
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" />
        </Field>
        <Field label={t("Databases")} hint={t("Comma-separated.")}>
          <input className="mono" value={databases} onChange={(e) => setDatabases(e.target.value)} placeholder="app, billing" />
        </Field>
        <Field label={t("Every")} hint={t("At least 1h, e.g. 24h.")}>
          <input className="mono" value={every} onChange={(e) => setEvery(e.target.value)} />
        </Field>
        <p className="muted small">{t("The source data is unmasked. It is written to disk only while it is loaded, then deleted.")}</p>
        <ErrorLine error={error} />
        <div className="form-actions">
          <button type="button" onClick={onClose}>
            {t("Cancel")}
          </button>
          <button className="primary" disabled={busy || !ok}>
            {t("Save")}
          </button>
        </div>
      </form>
    </Dialog>
  );
}
