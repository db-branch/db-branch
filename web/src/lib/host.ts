// apiHost is the host (and port) people put in commands that reach this
// server. It is the page address unless the page says otherwise (the
// console demo, which has no server, names an example host).
type Page = { dbbHost?: string; location: { host: string } };

export function apiHost(page: Page = window): string {
  return page.dbbHost ?? page.location.host;
}
