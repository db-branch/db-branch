import type { Setting } from "../api";

// What the form holds for a setting at first: a secret is write-only (the
// server never returns it), so its field starts empty.
export const initialValue = (s: Setting): string => (s.type === "secret" ? "" : s.value);

// editedValue is what a field keeps after an edit. Text is kept exactly as
// typed (trailing spaces and newlines count); emptied, it returns to the
// default. A secret is kept as typed too and trimmed when it is sent.
export const editedValue = (s: Setting, typed: string): string => (s.type === "secret" ? typed : typed || s.default);

export type SettingsChange = { values: Record<string, string>; clear: string[] };

// settingsChange is what to send: changed values, and the secrets to remove.
// A secret is changed only when something was typed (an empty one keeps its
// value on the server); typing a new value wins over a pending clear.
export function settingsChange(settings: Setting[], values: Record<string, string>, cleared: string[]): SettingsChange {
  const out: SettingsChange = { values: {}, clear: [] };
  for (const s of settings) {
    const v = values[s.key] ?? initialValue(s);
    if (s.type === "secret") {
      const typed = v.trim();
      if (typed) out.values[s.key] = typed;
      else if (cleared.includes(s.key)) out.clear.push(s.key);
    } else if (v !== s.value) {
      out.values[s.key] = v;
    }
  }
  return out;
}

export const hasChange = (c: SettingsChange): boolean => Object.keys(c.values).length > 0 || c.clear.length > 0;
