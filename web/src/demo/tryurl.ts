// Where the demo sends someone who wants to run db-branch themselves: the
// getting-started page (Multipass VM or their own Ubuntu), in the UI's language.
// The link goes through a path of the demo's own that redirects (demo-worker/_redirects),
// so the site's analytics can count the clicks by path.
import type { Language } from "../lib/i18n";

export function tryLocallyUrl(lang: Language): string {
  return lang === "ja" ? "/go/getting-started-ja" : "/go/getting-started";
}
