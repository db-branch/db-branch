import { describe, expect, it } from "vitest";
import { connectionFrom, hostPort, proxyWays, snippets } from "./connect";

const mysql = connectionFrom("mysql://feature-x:p%40ss@192.0.2.10:40001/");
const pg = connectionFrom("postgres://feature-x:s3cret@192.0.2.11:40002/postgres");

describe("connectionFrom", () => {
  it("URL をホスト・ポート・ユーザー・パスワードに分ける", () => {
    expect(mysql).toEqual({ engine: "mysql", host: "192.0.2.10", port: 40001, user: "feature-x", password: "p@ss", database: "", url: "mysql://feature-x:p%40ss@192.0.2.10:40001/" });
    expect(pg.engine).toBe("postgres");
    expect(pg.database).toBe("postgres");
  });
});

describe("snippets", () => {
  it("伏せ字のときはパスワードを出さず、コピー用には本物を入れる", () => {
    const s = snippets(mysql, false);
    expect(s.url.shown).toBe("mysql://feature-x:••••••@192.0.2.10:40001/");
    expect(s.url.copy).toBe("mysql://feature-x:p%40ss@192.0.2.10:40001/");
    expect(s.env.shown).not.toContain("p@ss");
    expect(s.env.copy).toContain("DB_PASSWORD=p@ss");
    expect(s.env.copy).toContain("DATABASE_URL=mysql://feature-x:p%40ss@192.0.2.10:40001/");
  });

  it("表示を選ぶと本物を見せる", () => {
    expect(snippets(mysql, true).url.shown).toBe(mysql.url);
  });

  it("mysql コマンドはパスワードを聞かせ、psql は URL を渡す", () => {
    expect(snippets(mysql, false).command.copy).toBe("mysql -h 192.0.2.10 -P 40001 -u feature-x -p");
    expect(snippets(pg, false).command.copy).toBe('psql "postgres://feature-x:s3cret@192.0.2.11:40002/postgres"');
    expect(snippets(pg, false).command.shown).toBe('psql "postgres://feature-x:••••••@192.0.2.11:40002/postgres"');
  });
});

describe("proxyWays", () => {
  it("ProxySQL 経由の接続先・ユーザー・クエリコメントを返す", () => {
    const p = { host: "proxy", port: 6033, user: "app.feature-x" };
    expect(hostPort(p)).toBe("proxy:6033");
    const w = proxyWays(p, "feature-x", "mysql");
    expect(w.command).toBe("mysql -h proxy -P 6033 -u app.feature-x -p");
    expect(w.comment).toBe("/*db_branch='feature-x'*/");
    expect(proxyWays(p, "feature-x", "postgres").command).toBe("psql -h proxy -p 6033 -U app.feature-x");
  });
});
