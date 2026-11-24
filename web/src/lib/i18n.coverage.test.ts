import { describe, expect, it } from "vitest";
import { ttlChoices } from "./branches";
import { ja } from "./ja";
import { resultLabel } from "./license";
import { PHASES } from "./upgrade";

const agentModulesFiles = import.meta.glob<string>("../../../internal/app/agent_modules.go", { query: "?raw", import: "default", eager: true });

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
  "Paid · needs license",
  "Renew the license automatically",
  "Anonymous creations per IP per hour",
  "Refuse changes that cannot be audited",
]);
// Short texts that are translated on purpose (confirmations, time/status phrases, connectives).
const JAPANESE_SHORT = new Set([
  "or",
  "Signed in as",
  "Creating {name}…",
  "Extended",
  "Reset started",
  "Forked from {name}",
  "Fork {name}",
  "Confirm delete",
  "Confirm reset",
  "Confirm restore",
  "Confirm upgrade",
  "No expiry",
  "Expires soon",
  "Expired {when}",
  "Expires {when}",
  "now",
  "{span} ago",
  "in {span}",
  "expired",
  "in <1m",
  "no expiry",
  "1 hour",
  "4 hours",
  "1 day",
  "3 days",
  "7 days",
  "30 days",
  "90 days",
  "1 year",
  "(since creation)",
  "{size} its own",
  " (cut)",
  "On {nodes}",
  "{node}: skipped ({reason})",
  "created {when}",
  "Update {name}",
  "Delete base {name}?",
  "last seen {when}",
  "{used} used of {total}",
  "Remove {name}?",
  "Saved checkpoint {name}",
  "Saved checkpoint",
  "Restore started",
  "Deleted checkpoint",
  "Checkpoints of {name}",
  "Restore {name}",
  "Delete {name}",
  "{count} transactions",
  "Restore to here",
  "Restore to #{tx}",
  "Upgraded to {to}",
  "Check found errors",
  "Check passed",
  "Rolled back",
  "Refused",
  "Not started",
  "Upgrade {name}",
  "Errors ignored",
  "Checking",
  "Stopping the server",
  "Starting the server",
  "Saving a Checkpoint",
  "Rolling back",
  "Working",
  "No upgrade target",
  "Merge into {name}",
  "until {date}",
  "covers {what}",
  "every module",
  "next check {when}",
  "{n} row{cut} · {time}",
  "{n} rows{cut} · {time}",
  "Renewed",
  "Subscription not active",
  "Failed",
  "Unknown",
  "Clear filters",
]);

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
  it("coreSettings のすべての Help 文言が ja.ts に訳を持つ", () => {
    const src = Object.values(agentModulesFiles)[0] ?? "";
    expect(src.length).toBeGreaterThan(0);
    const block = src.slice(src.indexOf("func coreSettings("), src.indexOf("func positiveDuration("));
    const missing: string[] = [];
    for (const m of block.matchAll(/Help:\s*"((?:[^"\\]|\\.)*)"/g)) {
      const help = JSON.parse(`"${m[1]}"`) as string;
      if (!(help in ja)) missing.push(help);
    }
    expect(missing).toEqual([]);
  });
  it("PHASES・resultLabel・ttlChoices のすべてが ja.ts に訳を持つ", () => {
    const keys = [
      ...Object.values(PHASES),
      ...ttlChoices.map(([, l]) => l),
      ...["renewed", "not_yet", "inactive", "error", "unknown"].map((r) => resultLabel(r)),
    ].filter(Boolean);
    expect(keys.filter((k) => !(k in ja))).toEqual([]);
  });
  it("UI コンポーネント・ページで生エラー文字列 e.message を直接 setError に渡していない", () => {
    const leaks: string[] = [];
    for (const [path, text] of Object.entries(files)) {
      if (/test\.tsx?$|\/(errors|sqlrun)\.ts$|\/demo\//.test(path)) continue;
      if (/set(?:Action|Check)?Error\([^)]*\.message\b/.test(text)) leaks.push(path);
      if (/\b(?:alert|confirm)\([^)]*\.message\b/.test(text)) leaks.push(`${path}: alert/confirm`);
      if (/(?:const|let)\s+err(?:or)?\s*=\s*\([^)]*as Error\)\.message/.test(text)) leaks.push(`${path}: err.message var`);
      if (/\bthrow new Error\(/.test(text)) leaks.push(`${path}: throw new Error`);
    }
    expect(leaks).toEqual([]);
  });
  it("Dialog・Drawer・Modal の title に t() を通さないテンプレートリテラルを使っていない", () => {
    const leaks: string[] = [];
    for (const [path, text] of Object.entries(files)) {
      if (/test\.tsx?$/.test(path)) continue;
      if (/<(?:Dialog|Drawer|Modal)[^>]*title=\{`/.test(text)) leaks.push(path);
    }
    expect(leaks).toEqual([]);
  });
  it("JSX 内に t() を通さない英語の文を残していない", () => {
    const leaks: string[] = [];
    // Match multi-word English sentences directly inside JSX tags >...<
    for (const [path, text] of Object.entries(files)) {
      if (!path.endsWith(".tsx") || /test\.tsx?$|\/demo\//.test(path)) continue;
      const stripped = text.replace(/\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");
      for (const m of stripped.matchAll(/>([^<>{}\n]+)</g)) {
        const s = m[1].trim();
        if (!s || /^[^A-Za-z]*$/.test(s)) continue;
        // Skip TypeScript/JS expression fragments that can appear between > and < in TSX source.
        if (
          /=>|===|!==|&&|\|\||[=;\[\]]|\b(?:Promise|RefObject|Record|Partial|Omit|Pick|Date\.parse|querySelectorAll)\b/.test(s) ||
          /^\s*(?:null|undefined|\?|:|\))/.test(s) ||
          /\)\s*:\s*$/.test(s) ||
          /\)\s*\?\s*$/.test(s)
        ) {
          continue;
        }
        if (!isShort(s) && !/^Ctrl\s*\/\s*⌘/.test(s) && !/^©\s*2026/.test(s) && !/^db-branch\b/.test(s)) {
          leaks.push(`${path}: ${s}`);
        }
      }
    }
    expect(leaks).toEqual([]);
  });
});
