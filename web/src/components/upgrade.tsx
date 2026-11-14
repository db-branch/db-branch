import { useEffect, useReducer, useRef, useState } from "react";
import { api, Branch } from "../api";
import { t } from "../lib/i18n";
import { canForce, countLevel, Finding, installHint, PHASES, resultTone, stepReducer, UpgradeResult, upgradeChoices } from "../lib/upgrade";
import { ErrorLine } from "./ui";

const POLL_MS = 1000;

// Findings: what the pre-check or the server's log said, errors first.
export function Findings({ title, list }: { title: string; list: Finding[] }) {
  if (list.length === 0) return null;
  const order = { error: 0, warning: 1, info: 2 };
  const sorted = [...list].sort((a, b) => order[a.level] - order[b.level]);
  return (
    <section className="findings" aria-label={title}>
      <h4 className="findings-title">{title}</h4>
      <ul>
        {sorted.map((f, i) => (
          <li key={i} className={`finding finding-${f.level}`}>
            <span className="finding-level">{f.level}</span>
            <span className="finding-text">{f.message}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}

// headline is the one sentence that says how the upgrade ended.
export function headline(r: UpgradeResult): string {
  const vars = { branch: r.branch, from: r.from, to: r.to, checkpoint: r.checkpoint ?? "" };
  switch (r.status) {
    case "upgraded":
      return t("{branch} now runs {to}. The state before is saved as the Checkpoint {checkpoint}; restore it to go back to {from}.", vars);
    case "checked":
      return countLevel(r.pre_check, "error") > 0
        ? t("Checked only, nothing was changed. The errors below stop the upgrade to {to}.", vars)
        : t("Checked only, nothing was changed. {branch} can move to {to}.", vars);
    case "rolled_back":
      return t("The upgrade to {to} failed. {branch} was rolled back to {from} (Checkpoint {checkpoint}).", vars);
    case "failed":
      return t("The upgrade to {to} failed and {branch} was left as it is. Restore the Checkpoint {checkpoint} to go back to {from}.", vars);
    case "refused":
      return t("The pre-check found errors, so {branch} was not changed.", vars);
    default:
      return t("The upgrade did not start; {branch} was not changed.", vars);
  }
}

// UpgradeResultView: how an upgrade ended, with the pre-check findings, the
// server log's warnings and the end of its error log.
export function UpgradeResultView({ result: r }: { result: UpgradeResult }) {
  return (
    <div className={`upgrade-result upgrade-${resultTone(r)}`}>
      <p role="status" className="upgrade-headline">
        {headline(r)}
      </p>
      {r.error && r.status !== "refused" && <p className="small upgrade-error">{r.error}</p>}
      <Findings title={t("Pre-check")} list={r.pre_check} />
      <Findings title={t("Server log during the upgrade")} list={r.log} />
      {r.log_excerpt && (
        <details className="upgrade-excerpt">
          <summary>{t("End of the server's error log")}</summary>
          <pre className="mono small">{r.log_excerpt}</pre>
        </details>
      )}
    </div>
  );
}

// UpgradePanel: choose a newer version for one Branch, confirm, watch it run
// and read the result. allow tells which actions the signed-in person may do.
export function UpgradePanel({ branch: b, ready, allow, onChange }: { branch: Branch; ready: boolean; allow: (action: string) => boolean; onChange: () => void }) {
  const [step, dispatch] = useReducer(stepReducer, { kind: "closed" });
  const [installed, setInstalled] = useState<string[] | null>(null);
  const [available, setAvailable] = useState<string[]>([]);
  const [target, setTarget] = useState("");
  const [error, setError] = useState<string | null>(null);
  const alive = useRef(true);
  const cancel = useRef<HTMLButtonElement>(null);
  useEffect(() => () => void (alive.current = false), []);

  const noRole = t("Your role cannot do this.");
  const why = !allow("branch.upgrade") ? noRole : !ready ? t("Available once it is ready.") : !b.checkpoints ? t("Upgrades need the node's own ZFS, like Checkpoints.") : undefined;

  useEffect(() => {
    if (step.kind !== "choose" || installed) return;
    // The catalog is what could be installed; without it the panel still lists what is installed.
    api.availableRuntimes().then(
      (rs) => alive.current && setAvailable(rs.map((r) => r.runtime)),
      () => undefined,
    );
    api.runtimes().then(
      (rs) => alive.current && setInstalled(rs.map((r) => r.runtime)),
      (e: Error) => alive.current && setError(e.message),
    );
  }, [step.kind, installed]);
  const { targets, toInstall } = installed ? upgradeChoices(b.runtime, installed, available) : { targets: [], toInstall: [] };
  useEffect(() => {
    if (!targets.includes(target)) setTarget(targets[targets.length - 1] ?? "");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [installed, b.runtime]);
  useEffect(() => {
    if (step.kind === "confirm") cancel.current?.focus();
  }, [step.kind]);

  const run = async (to: string, checkOnly: boolean, force: boolean) => {
    dispatch({ type: "run", target: to, checkOnly });
    setError(null);
    try {
      const started = await api.upgradeBranch(b.name, { runtime: to, check_only: checkOnly, force });
      for (;;) {
        const o = await api.operation(started.id);
        if (!alive.current) return;
        if (o.phase) dispatch({ type: "phase", phase: o.phase });
        if (o.state !== "running") {
          dispatch({ type: "finish", target: to, result: o.result ?? undefined, error: o.state === "failed" && !o.result ? o.error : undefined });
          break;
        }
        await new Promise((r) => setTimeout(r, POLL_MS));
      }
    } catch (e) {
      if (alive.current) dispatch({ type: "finish", target: to, error: (e as Error).message });
    }
    if (alive.current) onChange();
  };

  const button = (
    <button type="button" disabled={!!why || step.kind === "running"} title={why ?? t("Move this Branch to a newer version; the Base and other Branches stay as they are")} onClick={() => dispatch({ type: step.kind === "closed" || step.kind === "done" ? "open" : "close" })}>
      {t("Upgrade")}
    </button>
  );

  return (
    <span className="upgrade">
      {button}
      {step.kind === "choose" && (
        <div className="upgrade-box" role="group" aria-label={t("Upgrade {name}", { name: b.name })} onKeyDown={(e) => e.key === "Escape" && (e.stopPropagation(), dispatch({ type: "close" }))}>
          {!installed && !error && <p className="muted small">{t("Looking for installed versions…")}</p>}
          {installed && <NotInstalled runtime={b.runtime} targets={targets} toInstall={toInstall} />}
          {targets.length > 0 && (
            <>
              <label className="field-label" htmlFor={`up-${b.name}`}>
                {t("Upgrade to")}
              </label>
              <select id={`up-${b.name}`} className="mono" value={target} onChange={(e) => setTarget(e.target.value)}>
                {targets.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </select>
              <span className="upgrade-buttons">
                <button type="button" disabled={!target} onClick={() => run(target, true, false)}>
                  {t("Check only")}
                </button>
                <button type="button" className="primary" disabled={!target} onClick={() => dispatch({ type: "ask", target })}>
                  {t("Upgrade…")}
                </button>
              </span>
            </>
          )}
          <button type="button" className="text-button" onClick={() => dispatch({ type: "close" })}>
            {t("Close")}
          </button>
        </div>
      )}
      {step.kind === "confirm" && (
        <div className="confirm" role="group" aria-label={t("Upgrade {name}", { name: b.name })} onKeyDown={(e) => e.key === "Escape" && (e.stopPropagation(), dispatch({ type: "back" }))}>
          <span role="alert">
            {t("Upgrade {name} from {from} to {to}? A Checkpoint is saved first; on failure it rolls back automatically.", { name: b.name, from: b.runtime, to: step.target })}
            {step.force && <> {t("The pre-check found errors; they are ignored.")}</>}
          </span>
          <span className="confirm-buttons">
            <button type="button" ref={cancel} onClick={() => dispatch({ type: "back" })}>
              {t("Cancel")}
            </button>
            <button type="button" className="primary" onClick={() => run(step.target, false, step.force)}>
              {t("Confirm upgrade")}
            </button>
          </span>
        </div>
      )}
      {step.kind === "running" && (
        <p className="muted small" role="status">
          {step.checkOnly ? t("Checking") : t("Upgrading")}… {step.phase && t(PHASES[step.phase] ?? "Working")}
        </p>
      )}
      {step.kind === "done" && (
        <div className="upgrade-box">
          {step.result ? <UpgradeResultView result={step.result} /> : <ErrorLine error={step.error ?? null} />}
          <span className="upgrade-buttons">
            {step.result && canForce(step.result) && allow("branch.upgrade") && (
              <button type="button" onClick={() => dispatch({ type: "ask", target: step.target, force: true })}>
                {t("Upgrade anyway")}
              </button>
            )}
            {step.result?.status === "checked" && countLevel(step.result.pre_check, "error") === 0 && (
              <button type="button" className="primary" onClick={() => dispatch({ type: "ask", target: step.target })}>
                {t("Upgrade…")}
              </button>
            )}
            <button type="button" className="text-button" onClick={() => dispatch({ type: "close" })}>
              {t("Close")}
            </button>
          </span>
        </div>
      )}
      <ErrorLine error={error} />
    </span>
  );
}

// NotInstalled says which runtimes the branch could move to that are not installed
// on this node yet, with the command that installs each, or that there is none.
export function NotInstalled({ runtime, targets, toInstall }: { runtime: string; targets: string[]; toInstall: string[] }) {
  if (toInstall.length === 0) {
    return targets.length === 0 ? <p className="small">{t("No version newer than {runtime} can be reached in one step.", { runtime })}</p> : null;
  }
  return (
    <div className="small upgrade-install">
      <p>{targets.length === 0 ? t("None of the versions {runtime} can move to is installed on this node. Install one with:", { runtime }) : t("Can also be installed:")}</p>
      <ul>
        {toInstall.map((r) => (
          <li key={r}>
            <code className="mono">{installHint(r)}</code>
          </li>
        ))}
      </ul>
    </div>
  );
}
