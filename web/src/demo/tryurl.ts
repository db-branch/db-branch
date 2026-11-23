// Where the demo sends someone who wants to run db-branch themselves: the
// getting-started page (Multipass VM or their own Ubuntu), in the UI's language.
// ref=demo lets the site's analytics tell these visits apart.
import type { Language } from "../lib/i18n";

export function tryLocallyUrl(lang: Language): string {
  return `https://db-branch.com/${lang === "ja" ? "ja/" : ""}docs/getting-started/?ref=demo`;
}
