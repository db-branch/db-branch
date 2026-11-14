// The diff module's page in the console: when to show it and what to load. Pure.
import type { Capabilities } from "../api";
import { FrameProps, moduleFrame } from "./hooks";

// showDiff: the module has its page and the person may use its API.
export function showDiff(caps: Pick<Capabilities, "pages"> | undefined, can: (action: string) => boolean): boolean {
  return !!caps?.pages.some((p) => p.module === "diff") && can("ext.diff.read");
}

// diffFrame is the frame of the module's own page, with the two branches the
// address names (?a=…&b=…) passed on, so a link can open it with a branch chosen.
export function diffFrame(search: string, lang: string): FrameProps {
  const f = moduleFrame("diff", "/ui/", lang);
  const q = new URLSearchParams(search);
  const extra = (["a", "b"] as const).filter((k) => q.get(k)).map((k) => `${k}=${encodeURIComponent(q.get(k)!)}`);
  if (extra.length === 0 || !("src" in f)) return f;
  return { ...f, src: f.src + (f.src.includes("?") ? "&" : "?") + extra.join("&") };
}
