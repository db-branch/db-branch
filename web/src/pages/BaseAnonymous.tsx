import { useEffect, useState } from "react";
import { api, Base, Branch, canBase } from "../api";
import { ErrorLine, Info } from "../components/ui";
import { anonymousCount, changes, inputsOf } from "../lib/anonymous";
import { t } from "../lib/i18n";
import { useLive } from "../lib/live";

// AnonymousCard sets what people without an account may do with the base:
// how many branches they may hold from it and for how long. Which snapshots
// they may branch from is chosen above (Access: Public); the cluster-wide
// limits (Settings) apply as well.
export function AnonymousCard({ base, branches }: { base: Base; branches: Branch[] }) {
  const { data: offer } = useLive(api.publicBases, ["base", "branch"]);
  const current = base.anonymous ?? { max_branches: null, max_ttl: null };
  const key = JSON.stringify(current);
  const [max, setMax] = useState(inputsOf(current).max);
  const [ttl, setTtl] = useState(inputsOf(current).ttl);
  const [bad, setBad] = useState<"max" | "ttl" | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    setMax(inputsOf(current).max);
    setTtl(inputsOf(current).ttl);
    setBad(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  const manage = canBase(base, "base.publish");
  const parsed = changes(current, max, ttl);
  const dirty = !parsed.ok || Object.keys(parsed.body).length > 0;
  const anyPublic = base.snapshots.some((s) => s.public);
  const used = anonymousCount(branches, base.name);
  const cluster = offer?.anonymous;

  const save = async () => {
    if (!parsed.ok) {
      setBad(parsed.field);
      return;
    }
    setBad(null);
    setBusy(true);
    try {
      await api.setBaseAnonymous(base.name, parsed.body);
      setError(null);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="panel anonymous-card" aria-label={t("Without sign-in")}>
      <h2>
        {t("Without sign-in")}{" "}
        <Info label={t("About these limits")}>
          {t("Limits for people who create branches without an account, from this Base's public snapshots. Empty means inherit: only the cluster-wide limits apply. 0 allows none.")}
        </Info>
      </h2>
      <div className="anonymous-fields">
        <label className="field">
          <span className="field-label">{t("Max branches")}</span>
          <input className="mono" inputMode="numeric" value={max} placeholder="inherit" disabled={!manage} aria-invalid={bad === "max"} onChange={(e) => setMax(e.target.value)} />
        </label>
        <label className="field">
          <span className="field-label">{t("Max TTL")}</span>
          <input className="mono" value={ttl} placeholder="inherit" disabled={!manage} aria-invalid={bad === "ttl"} onChange={(e) => setTtl(e.target.value)} />
        </label>
        <div className="field">
          <span className="field-label">{t("In use")}</span>
          <span className="mono anonymous-count">{used}</span>
        </div>
        {manage && (
          <button onClick={save} disabled={!dirty || busy}>
            {t("Save")}
          </button>
        )}
      </div>
      {bad === "max" && <p className="field-hint bad">{t("Max branches: a whole number from 0, or empty to inherit.")}</p>}
      {bad === "ttl" && <p className="field-hint bad">{t("Max TTL: a duration such as 4h or 30m, or empty to inherit.")}</p>}
      <ErrorLine error={error} />
      {!anyPublic && <p className="muted small">{t("No snapshot is public, so nobody can branch from this Base without signing in yet.")}</p>}
      {cluster && (
        <p className="muted small">
          {t("Cluster-wide limits apply too: {count} branches in all, {ttl} at most, {rate} creations per IP per hour (0: no limit).", {
            count: cluster.max_branches,
            ttl: cluster.max_ttl,
            rate: cluster.rate_per_hour ?? "",
          })}{" "}
          <a href="/admin/settings">Settings</a>
        </p>
      )}
    </section>
  );
}
