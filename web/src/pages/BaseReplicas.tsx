import { useEffect, useState } from "react";
import { api, Base, canBase } from "../api";
import { ErrorLine } from "../components/ui";
import { useLive } from "../lib/live";
import { t } from "../lib/i18n";

// ReplicasCard shows which nodes hold the base and lets the caller choose
// which nodes keep a copy (the base's own node always does).
export function ReplicasCard({ base }: { base: Base }) {
  const { data: cluster } = useLive(api.cluster, ["node"]);
  const manage = canBase(base, "base.replicas");
  const current = base.replica_nodes ?? null;
  const [all, setAll] = useState(current === null);
  const [chosen, setChosen] = useState<string[]>(current ?? []);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const key = JSON.stringify(current);
  useEffect(() => {
    setAll(current === null);
    setChosen(current ?? []);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  const others = (cluster?.nodes ?? []).map((n) => n.name).filter((n) => n !== base.node);
  if (!cluster || cluster.nodes.length < 2) return null;
  const draining = (base.holders ?? []).filter((h) => h.state === "draining");
  const dirty = all !== (current === null) || (!all && [...chosen].sort().join(",") !== [...(current ?? [])].sort().join(","));
  const toggle = (n: string) => setChosen((c) => (c.includes(n) ? c.filter((x) => x !== n) : [...c, n]));
  const save = async () => {
    setBusy(true);
    try {
      await api.setBaseReplicas(base.name, all ? null : chosen);
      setError(null);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="panel replicas-card" aria-label={t("Replicas")}>
      <h2>{t("Replicas")}</h2>
      <dl className="kv">
        {(base.holders ?? []).map((h) => (
          <div key={h.node} style={{ display: "contents" }}>
            <dt className="mono">{h.node}</dt>
            <dd>
              <span className={`state-badge ${h.state === "held" ? "ok" : ""}`}>{h.origin ? t("origin") : t(h.state)}</span>
            </dd>
          </div>
        ))}
      </dl>
      {draining.length > 0 && <p className="muted">{t("These nodes give up their copy once no branch on them uses the base.")}</p>}
      {manage && (
        <>
          <div className="form">
            <label className="choice">
              <input type="radio" name={`replicas-${base.name}`} checked={all} onChange={() => setAll(true)} />
              <span>{t("All nodes")}</span>
            </label>
            <label className="choice">
              <input type="radio" name={`replicas-${base.name}`} checked={!all} onChange={() => setAll(false)} />
              <span>{t("Chosen nodes")}</span>
            </label>
            {!all &&
              others.map((n) => (
                <label key={n} className="choice indent mono">
                  <input type="checkbox" checked={chosen.includes(n)} onChange={() => toggle(n)} />
                  <span>{n}</span>
                </label>
              ))}
          </div>
          <ErrorLine error={error} />
          <div className="form-actions">
            <button onClick={save} disabled={!dirty || busy}>
              {t("Save")}
            </button>
          </div>
        </>
      )}
      <p className="muted">
        {t("Fewer copies save disk, but branches can only run on the nodes that hold the base.")}
      </p>
    </section>
  );
}
