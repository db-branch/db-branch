import { describe, expect, it } from "vitest";
import { ja } from "./ja";

// Policy: short labels (a word or a short noun phrase: headings, tabs, column
// names, buttons, badges, form labels) stay in English; only sentences are
// translated. A text is "short" when it has at most 3 words (a lone "·" does
// not count) and no sentence punctuation. ENGLISH_ONLY_EXTRA lists labels that
// are intentionally English although longer than that.
const ENGLISH_ONLY_EXTRA = new Set([
  "Email or user name",
  "Node holding the base",
  "Type the branch name",
  "Sign in with SSO",
  "name, e.g. my-laptop",
  "name, e.g. ci-deploy",
  "Largest dump upload (MB)",
  "Check for updates daily",
  "Scheduled refresh of {name}",
  "Paid · needs licence",
  "Renew the licence automatically",
]);
// Short texts that are translated on purpose (a connective, not a label).
const JAPANESE_SHORT = new Set(["or"]);

function isShort(text: string): boolean {
  if (ENGLISH_ONLY_EXTRA.has(text)) return true;
  const words = text.split(/\s+/).filter((w) => w !== "·");
  return words.length <= 3 && !/[.?!:;,](\s|$)|—|^,/.test(text);
}

// Every source file of the console as text (Vite inlines them for the test).
const files = import.meta.glob<string>("../**/*.{ts,tsx}", { query: "?raw", import: "default", eager: true });

describe("ja", () => {
  it("すべての文言に日本語がある（短いラベルは英語のまま）", () => {
    const missing = new Set<string>();
    for (const [path, text] of Object.entries(files)) {
      if (/test\.tsx?$|\/ja\.ts$/.test(path)) continue;
      for (const m of text.matchAll(/\bt\(\s*"((?:[^"\\]|\\.)*)"/g)) {
        const key = JSON.parse(`"${m[1]}"`) as string;
        if (!(key in ja) && !isShort(key) && !/^(INSERT|UPDATE)/.test(key)) missing.add(key);
      }
    }
    expect([...missing]).toEqual([]);
  });
  it("短いラベルを日本語にしていない", () => {
    const stale = Object.keys(ja).filter((k) => isShort(k) && !JAPANESE_SHORT.has(k));
    expect(stale).toEqual([]);
  });
});
