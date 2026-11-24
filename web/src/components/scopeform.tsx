import { FormEvent, ReactNode, useEffect, useState } from "react";
import { api, Setting, SettingScope } from "../api";
import { ErrorLine } from "./ui";
import { language, t } from "../lib/i18n";
import { groupSettings } from "../lib/scopegroups";
import { editedValue, hasChange, initialValue, settingsChange } from "../lib/settingsform";

// A setting's label and help in the console language: a module's own
// translation, else the console dictionary (core settings), else English.
const settingLabel = (s: Setting) => s.translations?.[language()]?.label || t(s.label || s.key);
const settingHelp = (s: Setting) => s.translations?.[language()]?.help || t(s.help ?? "");

// ScopeForm edits one scope; only changed values are sent, and an emptied
// field returns to the default (shown as placeholder). A secret is write-only:
// its field starts empty, an empty one keeps the stored value, and "Clear"
// removes it. Settings are grouped
// into collapsible sections by the first segment of their keys.
export function ScopeForm({ scope, bare }: { scope: SettingScope; bare?: boolean }) {
  const initial = Object.fromEntries(scope.settings.map((s) => [s.key, initialValue(s)]));
  const [values, setValues] = useState<Record<string, string>>(initial);
  const [cleared, setCleared] = useState<string[]>([]);
  const [error, setError] = useState<unknown>(null);
  const [saved, setSaved] = useState(false);
  // secret_set is part of the key: saving a secret changes only that.
  const key = JSON.stringify([initial, scope.settings.map((s) => !!s.secret_set)]);
  useEffect(() => {
    setValues(JSON.parse(key)[0]);
    setCleared([]);
  }, [key]);
  const change = settingsChange(scope.settings, values, cleared);
  const dirty = hasChange(change);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await api.putSettings(scope.scope, change.values, change.clear);
      setCleared([]);
      // A saved secret is not kept in the field (it is write-only).
      setValues((v) => ({ ...v, ...Object.fromEntries(scope.settings.filter((s) => s.type === "secret").map((s) => [s.key, ""])) }));
      setError(null);
      setSaved(true);
      setTimeout(() => setSaved(false), 1500);
    } catch (err) {
      setError(err);
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
          value={values[s.key] ?? ""}
          placeholder={s.secret_set ? t("Set") : s.default ? t("default") : t("none")}
          onChange={(v) => setValues({ ...values, [s.key]: editedValue(s, v) })}
          clearable={!!s.secret_set}
          clearing={cleared.includes(s.key) && !values[s.key]}
          onClear={(on) => setCleared(on ? [...cleared, s.key] : cleared.filter((k) => k !== s.key))}
        />
      ) : s.type === "text" ? (
        <textarea
          className="mono"
          rows={3}
          value={values[s.key] === s.default ? "" : values[s.key]}
          placeholder={s.default || t("none")}
          onChange={(e) => setValues({ ...values, [s.key]: editedValue(s, e.target.value) })}
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

// Secret is a masked input (keys, passwords) with a button to show it. A
// stored secret is never shown; "Clear" marks it for removal.
export function Secret({
  value,
  placeholder,
  onChange,
  clearable,
  clearing,
  onClear,
}: {
  value: string;
  placeholder: string;
  onChange: (v: string) => void;
  clearable: boolean;
  clearing: boolean;
  onClear: (on: boolean) => void;
}) {
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
      {clearable && (
        <button type="button" className="text-button" onClick={() => onClear(!clearing)} aria-pressed={clearing}>
          {clearing ? t("Will be cleared") : t("Clear")}
        </button>
      )}
    </span>
  );
}
