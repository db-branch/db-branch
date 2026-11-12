import { FormEvent, useState } from "react";
import { api, can, isAdmin, LicenseReport, ModuleState, SettingScope } from "../api";
import { ScopeForm } from "../components/scopeform";
import { ErrorLine } from "../components/ui";
import { relative } from "../lib/format";
import { billingLabel, Renewal, renewalInfo, resultLabel } from "../lib/license";
import { useLive } from "../lib/live";
import { t } from "../lib/i18n";
import { moduleLabel, planLabel } from "../lib/modules";

// Modules: /admin/modules is the overview (licence, and every module in the
// package repositories with Enable/Disable); /admin/modules/<name> is one
// enabled module's detail with its settings. Its pages are in the sidebar.
export function Modules({ name, onOpen }: { name?: string; onOpen?: (name: string) => void }) {
  const { data: scopes, error } = useLive(api.settings, ["setting", "module"]);
  const { data: modules, error: modError } = useLive(api.modules, ["module", "node"]);
  const { data: lic, reload: reloadLicense } = useLive(api.license, ["setting", "module"]);
  const [actionError, setActionError] = useState<string | null>(null);
  const run = (p: Promise<unknown>) => p.then(() => setActionError(null), (e: Error) => setActionError(e.message));

  const covered = (n: string) => !!lic?.report && (lic.report.all || lic.report.license.modules.includes(n)) && ["valid", "grace"].includes(lic.report.state);
  const available = modules?.available ?? {};
  const names = Array.from(new Set([...Object.keys(available), ...(modules?.items ?? []).map((m) => m.name)])).sort();
  const row = (n: string, detail: boolean) => {
    const state = modules?.items.find((m) => m.name === n);
    return (
      <ModuleRow
        key={n}
        name={n}
        description={available[n]}
        state={state}
        licensed={covered(n)}
        trial={!!lic?.report?.trial}
        scope={detail && state?.wanted ? scopes?.find((s) => s.scope === n) : undefined}
        onConfigure={!detail && state?.wanted ? () => onOpen?.(n) : undefined}
        onEnable={() => run(api.installModule(n))}
        onDisable={() => run(api.removeModule(n))}
      />
    );
  };
  return (
    <section className="page">
      <header className="page-head">
        <h1>{name ? moduleLabel(name) : t("Modules")}</h1>
      </header>
      <ErrorLine error={error ?? modError ?? actionError} />
      {name ? (
        <>
          <div className="modules">{row(name, true)}</div>
          {scopes && !scopes.some((s) => s.scope === name && s.settings.length > 0) && <p className="muted">{t("This module has no settings.")}</p>}
        </>
      ) : (
        <>
          <LicenseCard current={lic} onApplied={reloadLicense} paid={(modules?.items ?? []).filter((m) => m.requires_license).map((m) => m.name)} />
          {names.length === 0 && <p className="muted">{t("No modules in the configured package repositories.")}</p>}
          <div className="modules">{names.map((n) => row(n, false))}</div>
        </>
      )}
    </section>
  );
}

function ModuleRow({
  name,
  description,
  licensed,
  trial,
  state,
  scope,
  onConfigure,
  onEnable,
  onDisable,
}: {
  name: string;
  description?: string;
  licensed: boolean;
  trial: boolean;
  state?: ModuleState;
  scope?: SettingScope;
  onConfigure?: () => void;
  onEnable: () => void;
  onDisable: () => void;
}) {
  const on = !!state?.wanted;
  const nodes = state?.nodes ?? [];
  const done = nodes.filter((n) => n.current).length;
  return (
    <article className={`module ${on ? "" : "module-off"}`}>
      <header className="module-head">
        <span className="mono strong">{name}</span>
        {(state?.requires_license || (licensed && !trial)) && (
          <a
            href="/admin/modules#license"
            className={`paid-badge ${licensed ? "ok" : "need"}`}
            title={licensed ? (trial ? t("Works without a key during the trial") : t("Your licence covers this module")) : t("This module works only with a licence key that covers it")}
          >
            {licensed ? (trial ? t("Paid · trial") : t("Paid · licensed")) : t("Paid · needs licence")}
          </a>
        )}
        {on && <span className="mono muted">{state!.version}</span>}
        {description && <span className="muted module-desc">{description}</span>}
        {on && (
          <span className="holders" title={nodes.map((n) => `${n.node}: ${n.current ? "running" : n.loaded ? `running ${n.loaded}` : "installing"}`).join("\n")}>
            {nodes.map((n) => (
              <span key={n.node} className={n.current ? "held" : ""} />
            ))}
          </span>
        )}
        {on && done < nodes.length && (
          <span
            className="warn mono"
            title={nodes
              .filter((n) => !n.current)
              .map((n) => (n.loaded ? `${n.node} runs ${n.loaded}, not ${state!.version}` : `${n.node}: not installed yet`))
              .join("\n")}
          >
            {nodes.some((n) => !n.current && n.loaded) ? `other version on ${nodes.length - done}/${nodes.length}` : `installing ${done}/${nodes.length}`}
          </span>
        )}
        {on && nodes.some((n) => n.problem) && (
          <span className="warn" title={nodes.filter((n) => n.problem).map((n) => `${n.node}: ${n.problem}`).join("\n")}>
            {nodes.find((n) => n.problem)!.problem}
          </span>
        )}
        <span className="spacer" />
        {onConfigure && (
          <button className="text-button" onClick={onConfigure}>
            {t("Configure ›")}
          </button>
        )}
        {on ? <button onClick={onDisable}>{t("Disable")}</button> : <button onClick={onEnable}>{t("Enable")}</button>}
      </header>
      {scope && scope.settings.length > 0 && <ScopeForm scope={scope} bare />}
    </article>
  );
}


// LicenseCard is where paid modules are unlocked: paste the key, Verify;
// a good key is saved and the modules it covers are enabled on every node.
function LicenseCard({ current, paid, onApplied }: { current?: { configured: boolean; report?: LicenseReport; error?: string; renewal?: Renewal }; paid: string[]; onApplied: () => void }) {
  const [key, setKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<LicenseReport | null>(null);
  const [checking, setChecking] = useState(false);
  const [checkError, setCheckError] = useState<string | null>(null);
  const checkNow = async () => {
    setChecking(true);
    setCheckError(null);
    try {
      await api.checkLicenseNow();
    } catch (err) {
      setCheckError((err as Error).message);
    } finally {
      setChecking(false);
      onApplied();
    }
  };
  const verify = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      setResult(await api.applyLicense(key));
      setKey("");
      onApplied();
    } catch (err) {
      setError((err as Error).message + " The key was not saved.");
    } finally {
      setBusy(false);
    }
  };
  const trial = current?.report?.trial ? current.report : undefined;
  const rep = trial ? undefined : current?.report;
  const usable = rep && (rep.state === "valid" || rep.state === "grace");
  const renewal = rep ? renewalInfo(rep.license, current?.renewal, isAdmin() || can("license.write")) : undefined;
  const trialEnd = trial && new Date(trial.license.expires);
  const trialDays = trialEnd ? Math.max(0, Math.ceil((trialEnd.getTime() - Date.now()) / 86400000)) : 0;
  return (
    <form id="license" className="license-card" onSubmit={verify}>
      <div className="license-head">
        <h3>{t("License")}</h3>
        <span className="muted small">
          {t("Paid modules{list} work for 30 days without a key, then only with a licence key that covers them. Free modules need none.", { list: paid.length > 0 ? ` (${paid.join(", ")})` : "" })}
        </span>
      </div>
      <div className="license-status">
        {!current?.configured && !trial && <span className="muted">{t("No licence key. The 30-day trial begins when the first paid module is enabled.")}</span>}
        {trial && trial.state === "valid" && (
          <>
            <span className="state-badge ok">{t("trial")}</span>
            <span>{t("Every paid module works without a key until {date} ({n} days left).", { date: trialEnd!.toLocaleDateString(), n: trialDays })}</span>
          </>
        )}
        {trial && trial.state !== "valid" && <span className="warn">{t("The 30-day trial ended on {date}: paid modules are inactive until a licence key is saved.", { date: trialEnd!.toLocaleDateString() })}</span>}
        {current?.configured && current.error && <span className="warn">The saved key cannot be verified: {current.error}</span>}
        {rep && (
          <>
            <span className={`state-badge ${usable ? "ok" : "bad"}`}>{rep.state.replace("_", " ")}</span>
            <span>{rep.license.customer || rep.license.id}</span>
            <span className="plan-badge">{planLabel(rep.license.plan)}</span>
            {billingLabel(rep.license.billing) && <span className="plan-badge">{billingLabel(rep.license.billing)}</span>}
            <span className="muted">{t("until {date}", { date: new Date(rep.license.expires).toLocaleDateString() })}</span>
            {rep.license.max_nodes > 0 && <span className="muted">{t("up to {n} nodes", { n: rep.license.max_nodes })}</span>}
            <span className="muted">{t("covers {what}", { what: rep.all ? t("every module") : rep.license.modules.join(", ") })}</span>
          </>
        )}
      </div>
      {renewal?.subscribed && (
        <div className="license-status license-renewal">
          {renewal.auto ? (
            <>
              <span className="ok-line">{t("Renews automatically")}</span>
              {renewal.next && <span className="muted">{t("next check {when}", { when: relative(renewal.next) })}</span>}
              {renewal.result && (
                <span className={renewal.warn ? "warn" : "muted"}>
                  {t("last check: {result}", { result: t(resultLabel(renewal.result)) })}
                  {renewal.last && renewal.result !== "unknown" ? ` (${relative(renewal.last)})` : ""}
                </span>
              )}
              {renewal.warn && renewal.message && <span className="warn">{renewal.message}</span>}
              {renewal.canCheck && (
                <button type="button" disabled={checking} onClick={checkNow}>
                  {checking ? t("Checking…") : t("Check now")}
                </button>
              )}
            </>
          ) : (
            <span className="warn">{t("Automatic renewal is off. Paste the new key before {date}.", { date: new Date(rep!.license.expires).toLocaleDateString() })}</span>
          )}
          <a className="link-action" href="/admin/settings">
            {t("Auto-renew setting")}
          </a>
        </div>
      )}
      <ErrorLine error={checkError} />
      <div className="license-input">
        <span className="secret">
          <input className="mono" type="password" value={key} onChange={(e) => setKey(e.target.value)} placeholder={current?.configured ? t("Paste a new key to replace it") : t("Paste the licence key (dbbl1.…)")} autoComplete="off" spellCheck={false} />
        </span>
        <button className="primary" disabled={busy || !key.trim()}>
          {busy ? t("Verifying…") : t("Verify")}
        </button>
      </div>
      <ErrorLine error={error} />
      {result && (
        <p className="ok-line">
          {result.license.customer ? t("Verified for {who} and saved.", { who: result.license.customer }) : t("Verified and saved.")}{" "}
          {result.enabled.length > 0 ? t("Enabling {list} on every node.", { list: result.enabled.join(", ") }) : result.all ? t("Enable the modules you want below.") : t("Its modules are already on.")}
          {result.unavailable.length > 0 && ` Not in the package repositories: ${result.unavailable.join(", ")}.`}
        </p>
      )}
    </form>
  );
}
