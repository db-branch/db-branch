import { describe, expect, it } from "vitest";
import { MODULES, seed } from "../demo/state";
import { ttlChoices } from "./branches";
import { CLIENT_SPECS, generateClientOutput } from "./clientfiles";
import { connectionFrom } from "./connect";
import { ja } from "./ja";
import { resultLabel } from "./license";
import { tourSteps } from "./tour";
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
  "Scheduled refresh of {name}",
  "Paid · needs license",
  "Anonymous creations per IP per hour",
]);
// Short texts that are translated on purpose (confirmations, time/status phrases, connectives, tour/demo controls).
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
  "Initializing new cluster",
  "Checking cluster compatibility",
  "Dumping schemas",
  "Restoring schemas",
  "Swapping data directories",
  "Syncing data directory",
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
  // Tour headings & buttons
  "Choose a Base",
  "Name your Branch",
  "Set the lifetime",
  "Create it",
  "Your branches",
  "Skip tour",
  "Back",
  "Next",
  "Done",
  // Demo banner
  "View as",
  "Admin console",
  "Developer page",
  "Back to db-branch.com",
  // Module descriptions & cluster helpers
  "of {total}",
  "{n} base/branches",
  "Install on {nodes}",
  "Install on all",
  "Install on {n} missing",
  "Open {name}",
  "Access to {name}",
  "History of {branch}",
  "other version on {m}/{n}",
  "installing {done}/{total}",
  "{node}: not installed yet",
  "running",
  "running {version}",
  "installing",
  "Paused",
  "line {line}",
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
    const start = src.indexOf("func coreSettings(");
    const end = src.indexOf("func positiveDuration(");
    expect(start).toBeGreaterThanOrEqual(0);
    expect(end).toBeGreaterThan(start);
    const block = src.slice(start, end);
    const missing: string[] = [];
    let count = 0;
    for (const m of block.matchAll(/Help:\s*"((?:[^"\\]|\\.)*)"/g)) {
      count++;
      const help = JSON.parse(`"${m[1]}"`) as string;
      if (!(help in ja)) missing.push(help);
    }
    expect(count).toBeGreaterThan(0);
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
    // Match multi-word English sentences directly inside JSX tags >...< (including demo/Banner.tsx).
    for (const [path, text] of Object.entries(files)) {
      if (!path.endsWith(".tsx") || /test\.tsx?$/.test(path)) continue;
      if (/\/demo\//.test(path) && !path.endsWith("/Banner.tsx")) continue;
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
      // Also check static string attributes (double-quoted, single-quoted, or template literals outside t())
      for (const m of stripped.matchAll(/\b(?:placeholder|title|aria-label)\s*=\s*(?:"([^"]+)"|'([^']+)'|\{\s*`([^`]+)`\s*\})/g)) {
        const raw = (m[1] ?? m[2] ?? m[3] ?? "").replace(/\$\{[^}]*\}/g, " ").trim();
        if (!raw || !/[A-Za-z]/.test(raw) || /^[a-z0-9._-]+,\s*[a-z0-9._-]+$/i.test(raw)) continue;
        if (!isShort(raw)) leaks.push(`${path} [attr]: ${raw}`);
      }
    }
    expect(leaks).toEqual([]);
  });

  it("Tour・Modules・Demo 設定・ClientFilePanel のすべての文言が ja.ts に訳を持つ", () => {
    const missing: string[] = [];
    for (const step of tourSteps("alice-")) {
      if (!(step.title in ja)) missing.push(`tour title: ${step.title}`);
    }
    for (const btn of ["Skip tour", "Back", "Next", "Done"]) {
      if (!(btn in ja)) missing.push(`tour button: ${btn}`);
    }
    for (const txt of ["View as", "Admin console", "Developer page", "Back to db-branch.com"]) {
      if (!(txt in ja)) missing.push(`demo banner: ${txt}`);
    }
    for (const [name, mod] of Object.entries(MODULES)) {
      if (!(mod.description in ja)) missing.push(`module ${name}: ${mod.description}`);
    }
    const demoState = seed(Date.now());
    for (const scope of demoState.settings) {
      for (const s of scope.settings) {
        if (s.help && !(s.help in ja)) missing.push(`demo setting help (${scope.scope}.${s.key}): ${s.help}`);
        if (!isShort(s.label) && !(s.label in ja)) missing.push(`demo setting label (${scope.scope}.${s.key}): ${s.label}`);
      }
    }
    for (const b of demoState.bases) {
      if (b.description && !(b.description in ja)) missing.push(`demo base (${b.name}): ${b.description}`);
    }
    const mysqlConn = connectionFrom("mysql://u:p@127.0.0.1:3306/");
    const pgConn = connectionFrom("postgres://u:p@127.0.0.1:5432/postgres");
    for (const spec of CLIENT_SPECS) {
      if (!(spec.hintKey in ja)) missing.push(`client spec hint (${spec.id}): ${spec.hintKey}`);
      for (const conn of [mysqlConn, pgConn]) {
        if (spec.supportsClipboard(conn.engine)) {
          const out = generateClientOutput(spec.id, conn, "b");
          if (out.clipboard && !(out.clipboard.labelKey in ja) && !isShort(out.clipboard.labelKey)) {
            missing.push(`client clipboard (${spec.id}): ${out.clipboard.labelKey}`);
          }
        }
      }
    }
    expect(missing).toEqual([]);
  });

  it("demo/state.ts の core 設定 Help 文言が internal/app/agent_modules.go と一致する", () => {
    const src = Object.values(agentModulesFiles)[0] ?? "";
    expect(src.length).toBeGreaterThan(0);
    const start = src.indexOf("func coreSettings(");
    const end = src.indexOf("func positiveDuration(");
    expect(start).toBeGreaterThanOrEqual(0);
    expect(end).toBeGreaterThan(start);
    const block = src.slice(start, end);
    const coreHelp = new Map<string, string>();
    for (const m of block.matchAll(/Key:\s*(?:"([^"]+)"|licensing\.AutoRenewSetting)[\s\S]*?(?=Key:|^\t\})/gm)) {
      const key = m[1] ?? "license.auto_renew";
      const hm = m[0].match(/Help:\s*"((?:[^"\\]|\\.)*)"/);
      if (hm) coreHelp.set(key, JSON.parse(`"${hm[1]}"`) as string);
    }
    expect(coreHelp.size).toBeGreaterThan(0);
    const demoCore = seed(Date.now()).settings.find((s) => s.scope === "core")!;
    const mismatches: string[] = [];
    for (const s of demoCore.settings) {
      if (s.help && coreHelp.has(s.key) && s.help !== coreHelp.get(s.key)) {
        mismatches.push(`${s.key}: demo="${s.help}" vs go="${coreHelp.get(s.key)}"`);
      }
    }
    expect(mismatches).toEqual([]);
  });

  it("format.ts 以外で toLocaleString / toLocaleDateString / toLocaleTimeString を直接呼んでいない", () => {
    const leaks: string[] = [];
    for (const [path, text] of Object.entries(files)) {
      if (/test\.tsx?$|\/format\.ts$|\.\/format\.ts$|\/lib\/ja_render\.test\.tsx$/.test(path)) continue;
      if (/\.toLocale(?:Date|Time)?String\s*\(/.test(text)) {
        leaks.push(path);
      }
    }
    expect(leaks).toEqual([]);
  });
});
