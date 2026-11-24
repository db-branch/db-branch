import { FormEvent, useEffect, useState } from "react";
import { api, ApiError, Branch, Checkpoint } from "../api";
import { fullName, MAX_NAME, nameProblem } from "../lib/branchname";
import { t } from "../lib/i18n";
import { Dialog, ErrorLine, Field } from "./ui";

const ttls: [string, string][] = [
  ["1h", "1h"],
  ["4h", "4h"],
  ["24h", "1 day"],
  ["72h", "3 days"],
  ["168h", "7 days"],
  ["", "None"],
];

// ForkBranch opens a dialog to create an independent child branch from an
// existing branch's current state or one of its checkpoints.
export function ForkBranch({
  branch,
  checkpoints: initialList,
  initialCheckpoint = "",
  prefix,
  onClose,
  onForked,
}: {
  branch: Branch;
  checkpoints?: Checkpoint[];
  initialCheckpoint?: string;
  prefix?: string;
  onClose: () => void;
  onForked?: (name: string, operationId: string) => void;
}) {
  const [name, setName] = useState("");
  const [checkpoint, setCheckpoint] = useState(initialCheckpoint);
  const [checkpoints, setCheckpoints] = useState<Checkpoint[]>(initialList ?? []);
  const [ttl, setTtl] = useState("24h");
  const [access, setAccess] = useState<"private" | "open">("private");
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (initialList) return;
    let alive = true;
    api.checkpoints(branch.name).then(
      (list) => {
        if (alive) setCheckpoints(list);
      },
      () => undefined,
    );
    return () => {
      alive = false;
    };
  }, [branch.name, initialList]);

  const branchName = fullName(prefix, name);
  const problem = name ? nameProblem(prefix, name) : null;
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
            : undefined;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!name || problem || busy) return;
    setError(null);
    setBusy(true);
    try {
      const op = await api.forkBranch(branch.name, {
        name,
        checkpoint: checkpoint || undefined,
        ttl: ttl || undefined,
        access,
      });
      onForked?.(branchName, op.id);
      onClose();
    } catch (err) {
      setError(err instanceof ApiError && err.code === "already_exists" ? t("A branch named {name} already exists. Pick another name, or delete that branch first.", { name: branchName }) : err);
      setBusy(false);
    }
  };

  const extraInitialCheckpoint = initialCheckpoint && initialCheckpoint !== "@reset" && !checkpoints.some((c) => c.name === initialCheckpoint);

  return (
    <Dialog title={t("Fork {name}", { name: branch.name })} onClose={onClose}>
      <form className="form" onSubmit={submit}>
        <Field label={t("Name")} hint={hint}>
          <div className="name-input">
            {prefix && <span className="name-prefix mono" aria-hidden="true">{prefix}</span>}
            <input
              className="mono"
              aria-label={prefix ? `${t("Name")} (${prefix}…)` : undefined}
              aria-invalid={problem ? true : undefined}
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="experiment-1"
              autoFocus
              required
              maxLength={MAX_NAME}
            />
          </div>
        </Field>
        <Field label={t("From")}>
          <select value={checkpoint} onChange={(e) => setCheckpoint(e.target.value)}>
            <option value="">{t("Current state")}</option>
            <option value="@reset">@reset</option>
            {extraInitialCheckpoint && <option value={initialCheckpoint}>{initialCheckpoint}</option>}
            {checkpoints.map((c) => (
              <option key={c.name} value={c.name}>
                {c.name}
              </option>
            ))}
          </select>
        </Field>
        <Field label={t("TTL")}>
          <div className="segmented" role="radiogroup">
            {ttls.map(([v, l]) => (
              <button type="button" key={v} role="radio" aria-checked={ttl === v} className={ttl === v ? "on" : ""} onClick={() => setTtl(v)}>
                {l}
              </button>
            ))}
          </div>
        </Field>
        <Field label={t("Access")}>
          <div className="segmented" role="radiogroup">
            {(["private", "open"] as const).map((v) => (
              <button type="button" key={v} role="radio" aria-checked={access === v} className={access === v ? "on" : ""} onClick={() => setAccess(v)}>
                {v === "private" ? t("Private") : t("Anyone")}
              </button>
            ))}
          </div>
        </Field>
        <ErrorLine error={error} />
        <div className="form-actions">
          <button type="button" onClick={onClose}>
            {t("Cancel")}
          </button>
          <button type="submit" className="primary" disabled={busy || !name || !!problem}>
            Fork
          </button>
        </div>
      </form>
    </Dialog>
  );
}
