import { FormEvent, ReactNode, useEffect, useState } from "react";
import { api, Setting, SettingScope } from "../api";
import { ErrorLine } from "./ui";
import { language, t } from "../lib/i18n";
import { groupSettings } from "../lib/scopegroups";

// A setting's label and help in the console language: a module's own
// translation, else the console dictionary (core settings), else English.
const settingLabel = (s: Setting) => s.translations?.[language()]?.label || t(s.label || s.key);
const settingHelp = (s: Setting) => s.translations?.[language()]?.help || t(s.help ?? "");

// ScopeForm edits one scope; only changed values are sent, and an emptied
// field returns to the default (shown as placeholder). Settings are grouped
// into collapsible sections by the first segment of their keys.
export function ScopeForm({ scope, bare }: { scope: SettingScope; bare?: boolean }) {
  const initial = Object.fromEntries(scope.settings.map((s) => [s.key, s.value]));
  const [values, setValues] = useState<Record<string, string>>(initial);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const key = JSON.stringify(initial);
  useEffect(() => setValues(JSON.parse(key)), [key]);
  const changed = Object.fromEntries(Object.entries(values).filter(([k, v]) => v !== initial[k]));
  const dirty = Object.keys(changed).length > 0;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await api.putSettings(scope.scope, changed);
      setError(null);
      setSaved(true);
      setTimeout(() => setSaved(false), 1500);
    } catch (err) {
      setError((err as Error).message);
    }
  };
  const field = (s: Setting) => (
    <label key={s.key} className="setting">
      <span className="setting-label">
        {settingLabel(s)}
        {s.help && <span className="field-hint">{settingHelp(s)}</span>}
      </span>
      {s.type === "secret" ? (
        <Secret
          value={values[s.key] === s.default ? "" : values[s.key]}
          placeholder={s.default ? t("default") : t("none")}
          onChange={(v) => setValues({ ...values, [s.key]: v.trim() || s.default })}
        />
      ) : s.type === "text" ? (
        <textarea
          className="mono"
          rows={3}
          value={values[s.key] === s.default ? "" : values[s.key]}
          placeholder={s.default || t("none")}
          onChange={(e) => setValues({ ...values, [s.key]: e.target.value.trim() || s.default })}
        />
      ) : s.type === "bool" ? (
        <input type="checkbox" checked={(values[s.key] || s.default) === "true"} onChange={(e) => setValues({ ...values, [s.key]: String(e.target.checked) })} />
      ) : (
        <input
          className="mono"
          inputMode={s.type === "int" ? "numeric" : undefined}
          value={values[s.key] === s.default ? "" : values[s.key]}
          placeholder={s.default || (s.type === "duration" ? t("none") : "")}
          onChange={(e) => setValues({ ...values, [s.key]: e.target.value || s.default })}
        />
      )}
    </label>
  );
  return (
    <form className={bare ? "settings-form" : "settings-form settings-card"} onSubmit={submit}>
      {!bare && <h2 className="section-title">{t(scope.title)}</h2>}
      {groupSettings(scope.settings).map((g) =>
        g.label ? (
          <details key={g.prefix} className="settings-group" open>
            <summary>
              <Chevron />
              {t(g.label)}
            </summary>
            <div className="settings-group-body">{g.items.map(field)}</div>
          </details>
        ) : (
          <div key={g.prefix} className="settings-group-body">
            {g.items.map(field)}
          </div>
        ),
      )}
      <ErrorLine error={error} />
      <div className="form-actions">
        {saved && <span className="muted">{t("Saved")}</span>}
        <button className="primary" disabled={!dirty}>
          {t("Save")}
        </button>
      </div>
    </form>
  );
}

/** A small chevron that points right and turns down when its parent is open (see styles.css). */
export function Chevron(): ReactNode {
  return (
    <svg className="chevron" width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
      <path d="M3 1.5 6.5 5 3 8.5" />
    </svg>
  );
}

// Secret is a masked input (keys, passwords) with a button to show it.
function Secret({ value, placeholder, onChange }: { value: string; placeholder: string; onChange: (v: string) => void }) {
  const [shown, setShown] = useState(false);
  return (
    <span className="secret">
      <input
        className="mono"
        type={shown ? "text" : "password"}
        value={value}
        placeholder={placeholder}
        autoComplete="off"
        spellCheck={false}
        onChange={(e) => onChange(e.target.value)}
      />
      <button type="button" className="text-button" onClick={() => setShown(!shown)} aria-pressed={shown} title={shown ? t("Hide") : t("Show")}>
        {shown ? t("Hide") : t("Show")}
      </button>
    </span>
  );
}
