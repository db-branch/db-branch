// Connection details of a branch, from its connection URL, and the ways
// people paste them: the URL, a client command, .env lines.

export type Connection = { engine: string; host: string; port: number; user: string; password: string; database: string; url: string };

export function connectionFrom(url: string): Connection {
  const u = new URL(url);
  return {
    engine: u.protocol.replace(/:$/, ""),
    host: u.hostname,
    port: Number(u.port),
    user: decodeURIComponent(u.username),
    password: decodeURIComponent(u.password),
    database: decodeURIComponent(u.pathname.replace(/^\//, "")),
    url,
  };
}

const MASK = "••••••";

export type Snippet = { shown: string; copy: string };

// snippets returns each form twice: as shown (password masked unless
// reveal) and as copied (always the real password).
export function snippets(c: Connection, reveal: boolean): { url: Snippet; command: Snippet; env: Snippet } {
  const withPw = (pw: string) => c.url.replace(/^([a-z]+:\/\/[^:@/]*:)[^@]*@/, `$1${pw}@`);
  const shownUrl = reveal ? c.url : withPw(MASK);
  const command: Snippet =
    c.engine === "postgres"
      ? { shown: `psql "${shownUrl}"`, copy: `psql "${c.url}"` }
      : { shown: `mysql -h ${c.host} -P ${c.port} -u ${c.user} -p`, copy: `mysql -h ${c.host} -P ${c.port} -u ${c.user} -p` };
  const env = (url: string, pw: string) =>
    [`DATABASE_URL=${url}`, `DB_HOST=${c.host}`, `DB_PORT=${c.port}`, `DB_USER=${c.user}`, `DB_PASSWORD=${pw}`, ...(c.database ? [`DB_NAME=${c.database}`] : [])].join("\n");
  return {
    url: { shown: shownUrl, copy: c.url },
    command,
    env: { shown: env(shownUrl, reveal ? c.password : MASK), copy: env(c.url, c.password) },
  };
}

export const hostPort = (c: { host: string; port: number }) => `${c.host}:${c.port}`;

// proxyWays: reaching a branch through ProxySQL: by user name, or by the
// query comment that routes one connection to the branch.
export function proxyWays(proxy: { host: string; port: number; user: string }, branch: string, engine: string) {
  return {
    hostPort: hostPort(proxy),
    user: proxy.user,
    command: engine === "postgres" ? `psql -h ${proxy.host} -p ${proxy.port} -U ${proxy.user}` : `mysql -h ${proxy.host} -P ${proxy.port} -u ${proxy.user} -p`,
    comment: `/*db_branch='${branch}'*/`,
  };
}
