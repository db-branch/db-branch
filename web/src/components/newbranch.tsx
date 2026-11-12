import { FormEvent, useId, useRef, useState } from "react";
import { api, ApiError } from "../api";
import { BasePicker } from "./basepicker";
import { Job } from "./branchcard";
import { ErrorLine } from "./ui";
import { relative } from "../lib/format";
import { arrange, Choice, initialBase, loadRecent, pushRecent, saveRecent } from "../lib/bases";
import { t } from "../lib/i18n";
import { hours, ttlChoices } from "../lib/branches";
import { fullName, MAX_NAME, nameProblem, suggestName } from "../lib/branchname";

// What branches made without signing in may do (cluster-wide, or one base's).
export type Limits = { max_ttl: string; max_branches: number };


// NewBranchSkeleton holds the panel's place while the bases load, so nothing jumps.
export function NewBranchSkeleton() {
  return (
    <div className="new-branch panel" aria-hidden="true">
      <div className="panel-body">
        <div className="skeleton skeleton-bar" />
        <div className="skeleton skeleton-list" />
      </div>
      <div className="panel-foot">
        <div className="skeleton skeleton-bar" />
        <div className="skeleton skeleton-bar" />
        <div className="skeleton skeleton-bar" />
      </div>
    </div>
  );
}

export function NewBranch({
  choices,
  signedIn,
  prefix,
  limits: clusterLimits,
  perBase,
  onStarted,
}: {
  choices: Choice[];
  signedIn: boolean;
  prefix?: string;
  limits?: Limits;
  perBase?: Record<string, Limits>;
  onStarted: (job: Job, operation: string) => void;
}) {
  const [recent, setRecent] = useState<string[]>(loadRecent);
  const [base, setBase] = useState(() => initialBase(arrange(choices, recent), recent));
  const [name, setName] = useState(() => suggestName(base, prefix));
  // The suggestion in the box; as long as the name is still it, a new Base replaces it.
  const [suggested, setSuggested] = useState(name);
  const nameInput = useRef<HTMLInputElement>(null);
  const [ttl, setTtl] = useState("4h");
  const [access, setAccess] = useState<"private" | "open">("private");
  const [error, setError] = useState<string | null>(null);
  const [starting, setStarting] = useState(false);
  const ids = { name: useId(), hint: useId(), ttl: useId(), access: useId() };
  const limits = perBase?.[base] ?? clusterLimits;
  const maxH = limits ? hours(limits.max_ttl) : Infinity;
  const ttls = ttlChoices.filter(([v]) => hours(v) <= maxH);
  const branchName = fullName(prefix, name);
  const problem = nameProblem(prefix, name);
  const chosen = choices.find((c) => c.name === base) ?? arrange(choices, recent)[0];

  const suggest = (b: string) => {
    const s = suggestName(b, prefix);
    setName(s);
    setSuggested(s);
    setError(null);
  };
  const pick = (b: string) => {
    setBase(b);
    if (name === "" || name === suggested) suggest(b);
  };

  const hint =
    problem === "long"
      ? prefix
        ? t("Too long: after {prefix} you may type at most {max} characters.", { prefix, max: MAX_NAME - prefix.length })
        : t("Too long: at most {max} characters.", { max: MAX_NAME })
      : problem === "start"
        ? t("Start with a lowercase letter.")
        : problem === "chars"
          ? t("Use lowercase letters, digits and single hyphens; do not end with a hyphen.")
          : branchName && prefix
            ? t("The branch will be named {name}.", { name: branchName })
            : "";

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!name || problem || starting) return;
    setError(null);
    setStarting(true);
    const made = branchName;
    try {
      const op = await api.createBranch({ name, base: chosen.name, ttl, access: signedIn ? access : undefined });
      const next = pushRecent(recent, chosen.name);
      setRecent(next);
      saveRecent(next);
      suggest(chosen.name);
      onStarted({ name: made, base: chosen.name, runtime: chosen.runtime, startedAt: Date.now(), state: "running" }, op.id);
    } catch (err) {
      // The server's text is English and names the rule; say it in the page's language.
      setError(err instanceof ApiError && err.code === "already_exists" ? t("A branch named {name} already exists. Pick another name, or delete that branch first.", { name: made }) : (err as Error).message);
    } finally {
      setStarting(false);
    }
  };

  return (
    <form className="new-branch panel" id="new-branch" onSubmit={submit}>
      <div className="panel-body">
        <BasePicker
          choices={choices}
          recent={recent}
          value={chosen.name}
          onChange={pick}
          onPicked={() => {
            nameInput.current?.focus();
            nameInput.current?.select();
          }}
        />
      </div>
      <div className="panel-foot">
        <p className="picked small">
          <span className="muted">{t("From")}</span> <span className="mono strong">{chosen.name}</span>
          {chosen.snapshotAt && <span className="muted"> · Snapshot {chosen.snapshot}, {relative(chosen.snapshotAt)}</span>}
        </p>
        <div className="field">
          <label className="field-label" htmlFor={ids.name}>
            {t("Name")}
          </label>
          <div className="name-input" data-tour="name">
            {prefix && <span className="name-prefix mono" aria-hidden="true">{prefix}</span>}
            <input
              id={ids.name}
              ref={nameInput}
              className="mono"
              aria-label={prefix ? `${t("Name")} (${prefix}…)` : undefined}
              aria-describedby={ids.hint}
              aria-invalid={problem ? true : undefined}
              value={name}
              onChange={(e) => (setName(e.target.value), setError(null))}
              placeholder="my-feature"
              maxLength={MAX_NAME}
              autoComplete="off"
              spellCheck={false}
              required
            />
            <button type="button" className="icon-button regenerate" aria-label={t("Suggest another name")} title={t("Suggest another name")} onClick={() => suggest(chosen.name)}>
              <span aria-hidden="true">↻</span>
            </button>
          </div>
          <span id={ids.hint} className={`field-hint ${problem ? "bad" : ""}`} role={problem ? "alert" : undefined}>
            {hint}
          </span>
        </div>
        <div className="options">
          <div className="field" role="group" aria-labelledby={ids.ttl} data-tour="ttl">
            <span className="field-label" id={ids.ttl}>
              {t("Keep for")}
            </span>
            <div className="segmented">
              {ttls.map(([v, l]) => (
                <button type="button" key={v} aria-pressed={ttl === v} className={ttl === v ? "on" : ""} onClick={() => setTtl(v)}>
                  {t(l)}
                </button>
              ))}
            </div>
          </div>
          {signedIn ? (
            <div className="field" role="group" aria-labelledby={ids.access}>
              <span className="field-label" id={ids.access}>
                {t("Who can connect")}
              </span>
              <div className="segmented" title={access === "private" ? t("Only you, with a password of its own.") : t("Anyone, with the shared password.")}>
                <button type="button" aria-pressed={access === "private"} className={access === "private" ? "on" : ""} onClick={() => setAccess("private")}>
                  {t("Only me")}
                </button>
                <button type="button" aria-pressed={access === "open"} className={access === "open" ? "on" : ""} onClick={() => setAccess("open")}>
                  {t("Anyone")}
                </button>
              </div>
            </div>
          ) : null}
        </div>
        {signedIn ? (
          access === "open" && <p className="muted small access-note">{t("Anyone, with the shared password.")}</p>
        ) : (
          <p className="muted small">
            {t("Without signing in, branches are open to anyone with the shared password, last at most {hours} hours, and only {count} may exist at once.", {
              hours: limits ? +maxH.toFixed(2) : "",
              count: limits?.max_branches ?? "",
            })}
          </p>
        )}
        <ErrorLine error={error} />
        <button className="primary create" data-tour="create" disabled={!name || !!problem || starting}>
          {starting ? t("Creating…") : t("Create branch")}
        </button>
      </div>
    </form>
  );
}
