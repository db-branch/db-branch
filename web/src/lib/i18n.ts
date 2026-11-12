// i18n: English text is the key; ja.ts holds the Japanese. A text without
// a translation shows in English. {name} in a text is filled from vars.
import { ja } from "./ja";

export type Language = "en" | "ja";
export const languages: [Language | "", string][] = [
  ["", "Browser default"],
  ["en", "English"],
  ["ja", "日本語"],
];

const dictionaries: Record<Language, Record<string, string>> = { en: {}, ja };
let current: Language = "en";

export function setLanguage(l: Language) {
  current = l;
  if (typeof document !== "undefined") document.documentElement.lang = l;
}

export function language(): Language {
  return current;
}

// chooseLanguage: the person's choice, else the browser's, else English.
export function chooseLanguage(chosen: string | undefined, browser: string | undefined): Language {
  if (chosen === "en" || chosen === "ja") return chosen;
  return browser?.toLowerCase().startsWith("ja") ? "ja" : "en";
}

const KEY = "db-branch.language";

// remembered is the choice made on this browser before signing in.
export function remembered(): string {
  try {
    return localStorage.getItem(KEY) ?? "";
  } catch {
    return "";
  }
}

export function remember(l: string) {
  try {
    localStorage.setItem(KEY, l);
  } catch {
    /* storage unavailable */
  }
}

export function t(text: string, vars?: Record<string, string | number>): string {
  let s = dictionaries[current][text] ?? text;
  if (vars) for (const [k, v] of Object.entries(vars)) s = s.split(`{${k}}`).join(String(v));
  return s;
}
