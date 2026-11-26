import { describe, expect, it } from "vitest";
import { connectionFrom } from "./connect";
import { CLIENT_SPECS, clientSpecsFor, connectionForProxy, generateClientOutput, sanitizeFilename } from "./clientfiles";
import { ja } from "./ja";

const mysql = connectionFrom("mysql://alice-feat:p%40ss%26w%3C0%3Erd@192.0.2.10:40001/");
const pg = connectionFrom("postgres://alice-feat:s3cret%2B1@192.0.2.11:40002/postgres");
const mariadb = connectionFrom("mariadb://alice-feat:m%40ria@192.0.2.12:40003/app");
const custom = connectionFrom("clickhouse://alice-feat:chpass@192.0.2.13:9000/default");

describe("connectionForProxy", () => {
  it("replaces host, port, and user while preserving password and engine", () => {
    const px = connectionForProxy(mysql, { host: "proxy.example.com", port: 6033, user: "alice-feat" });
    expect(px.engine).toBe("mysql");
    expect(px.host).toBe("proxy.example.com");
    expect(px.port).toBe(6033);
    expect(px.user).toBe("alice-feat");
    expect(px.password).toBe("p@ss&w<0>rd");
    expect(px.url).toBe("mysql://alice-feat:p%40ss%26w%3C0%3Erd@proxy.example.com:6033/");
  });

  it("brackets IPv6 hosts in proxy URLs", () => {
    const px = connectionForProxy(mysql, { host: "::1", port: 6033, user: "u" });
    expect(px.host).toBe("::1");
    expect(px.url).toContain("@[::1]:6033/");
  });
});

describe("clientSpecsFor", () => {
  it("lists MySQL-compatible clients (excluding pgAdmin)", () => {
    const ids = clientSpecsFor("mysql").map((c) => c.id);
    expect(ids).toEqual(["dbeaver", "chat2db", "datagrip", "navicat", "workbench", "tableplus"]);
  });

  it("lists PostgreSQL-compatible clients (excluding MySQL Workbench)", () => {
    const ids = clientSpecsFor("postgres").map((c) => c.id);
    expect(ids).toEqual(["dbeaver", "chat2db", "datagrip", "navicat", "pgadmin", "tableplus"]);
  });

  it("lists generic URL clients (and Chat2DB for ClickHouse) for unknown external runtimes without crashing", () => {
    const ids = clientSpecsFor("clickhouse").map((c) => c.id);
    expect(ids).toEqual(["chat2db", "datagrip", "tableplus"]);
    expect(generateClientOutput("datagrip", custom, "alice-ch").file).toBeUndefined();
    expect(generateClientOutput("datagrip", custom, "alice-ch").clipboard?.text).toBe("jdbc:clickhouse://192.0.2.13:9000/default?user=alice-feat");
    const chChat2db = JSON.parse(generateClientOutput("chat2db", custom, "alice-ch").file!.content);
    expect(chChat2db[0].type).toBe("CLICKHOUSE");
  });
});

describe("generateClientOutput", () => {
  it("omits passwords from all outputs by default", () => {
    for (const spec of clientSpecsFor("mysql")) {
      const out = generateClientOutput(spec.id, mysql, "alice-feat");
      if (out.file) expect(out.file.content).not.toContain("p@ss&w<0>rd");
      if (out.clipboard) expect(out.clipboard.text).not.toContain("p@ss&w<0>rd");
    }
    for (const spec of clientSpecsFor("postgres")) {
      const out = generateClientOutput(spec.id, pg, "alice-feat");
      if (out.file) expect(out.file.content).not.toContain("s3cret+1");
      if (out.clipboard) expect(out.clipboard.text).not.toContain("s3cret+1");
    }
  });

  it("generates valid DBeaver .dbp project archive and data-sources.json preview for MySQL, PostgreSQL, MariaDB, and ProxySQL", () => {
    const myOut = generateClientOutput("dbeaver", mysql, "alice/feat");
    expect(myOut.file?.filename).toBe("alice-feat.dbp");
    expect(myOut.file?.bytes).toBeInstanceOf(Uint8Array);
    expect(myOut.file?.bytes?.subarray(0, 2)).toEqual(new Uint8Array([0x50, 0x4b]));
    const myJson = JSON.parse(myOut.file!.content);
    const myEntry = Object.values(myJson.connections)[0] as any;
    expect(myEntry.provider).toBe("mysql");
    expect(myEntry.driver).toBe("mysql8");
    expect(myEntry["save-password"]).toBe(false);
    expect(myEntry.configuration).toMatchObject({
      host: "192.0.2.10",
      port: "40001",
      user: "alice-feat",
      url: "jdbc:mysql://192.0.2.10:40001/",
      configurationType: "MANUAL",
      "auth-model": "native",
    });
    expect(myEntry.configuration.password).toBeUndefined();

    const myWithPwOut = generateClientOutput("dbeaver", mysql, "alice-feat", { includePassword: true, viaProxy: true });
    expect(myWithPwOut.file?.filename).toBe("alice-feat-proxysql.dbp");
    const myWithPw = JSON.parse(myWithPwOut.file!.content);
    const myPwEntry = Object.values(myWithPw.connections)[0] as any;
    expect(myPwEntry.name).toBe("alice-feat (ProxySQL)");
    expect(myPwEntry["save-password"]).toBe(true);
    expect(myPwEntry.configuration.password).toBe("p@ss&w<0>rd");
    // Verify the embedded data-sources.json inside .dbp sets save-password: true without inline plaintext password.
    const dsText = new TextDecoder().decode(myWithPwOut.file!.bytes!);
    expect(dsText).toContain('"save-password": true');
    expect(dsText).not.toContain("p@ss&w<0>rd");

    const pgJson = JSON.parse(generateClientOutput("dbeaver", pg, "alice-feat").file!.content);
    const pgEntry = Object.values(pgJson.connections)[0] as any;
    expect(pgEntry.provider).toBe("postgresql");
    expect(pgEntry.driver).toBe("postgres-jdbc");
    expect(pgEntry.configuration.database).toBe("postgres");
    expect(pgEntry.configuration.url).toBe("jdbc:postgresql://192.0.2.11:40002/postgres");

    const maJson = JSON.parse(generateClientOutput("dbeaver", mariadb, "alice-feat").file!.content);
    const maEntry = Object.values(maJson.connections)[0] as any;
    expect(maEntry.provider).toBe("mysql");
    expect(maEntry.driver).toBe("mariaDB");
  });

  it("generates valid Chat2DB JSON array without password", () => {
    const out = generateClientOutput("chat2db", mysql, "aaa1", { includePassword: true });
    expect(out.file?.filename).toBe("aaa1-chat2db.json");
    const arr = JSON.parse(out.file!.content);
    expect(arr).toEqual([
      {
        alias: "aaa1",
        type: "MYSQL",
        host: "192.0.2.10",
        port: "40001",
        url: "jdbc:mysql://192.0.2.10:40001/",
        user: "alice-feat",
        ssh: { use: false },
      },
    ]);

    const pgArr = JSON.parse(generateClientOutput("chat2db", pg, "pg1", { viaProxy: true }).file!.content);
    expect(pgArr[0].alias).toBe("pg1 (ProxySQL)");
    expect(pgArr[0].type).toBe("POSTGRESQL");
    expect(pgArr[0].url).toBe("jdbc:postgresql://192.0.2.11:40002/postgres");
  });

  it("generates valid DataGrip dataSources.xml and JDBC URL", () => {
    const myOut = generateClientOutput("datagrip", mysql, "alice-feat");
    expect(myOut.file?.filename).toBe("dataSources-alice-feat.xml");
    expect(myOut.file?.content).toContain('<component name="DataSourceManagerImpl" format="xml" multifile-model="true">');
    expect(myOut.file?.content).toContain("<driver-ref>mysql.8</driver-ref>");
    expect(myOut.file?.content).toContain("<jdbc-driver>com.mysql.cj.jdbc.Driver</jdbc-driver>");
    expect(myOut.file?.content).toContain("<jdbc-url>jdbc:mysql://192.0.2.10:40001/</jdbc-url>");
    expect(myOut.file?.content).toContain("<user-name>alice-feat</user-name>");
    expect(myOut.clipboard?.text).toBe("jdbc:mysql://192.0.2.10:40001/?user=alice-feat");

    const pgWithPw = generateClientOutput("datagrip", pg, "alice-feat", { includePassword: true });
    expect(pgWithPw.file?.content).not.toContain("s3cret+1");
    expect(pgWithPw.file?.content).toContain("<driver-ref>postgresql</driver-ref>");
    expect(pgWithPw.file?.content).toContain("<jdbc-driver>org.postgresql.Driver</jdbc-driver>");
    expect(pgWithPw.clipboard?.text).toBe("jdbc:postgresql://192.0.2.11:40002/postgres?user=alice-feat&password=s3cret%2B1");
  });

  it("generates XML-escaped Navicat .ncx without plaintext password", () => {
    const out = generateClientOutput("navicat", mysql, 'a&b<"c">');
    expect(out.file?.filename).toBe("a-b--c--.ncx");
    expect(out.file?.content).toContain('<Connections Ver="1.5">');
    expect(out.file?.content).toContain('ConnectionName="a&amp;b&lt;&quot;c&quot;&gt;"');
    expect(out.file?.content).toContain('ConnType="MYSQL"');
    expect(out.file?.content).toContain('SSH="false"');
    expect(out.file?.content).toContain('SavePassword="false"');
    expect(out.file?.content).not.toContain(" Password=");

    const pgOut = generateClientOutput("navicat", pg, "pg-branch", { includePassword: true });
    expect(pgOut.file?.content).toContain('ConnType="POSTGRESQL"');
    expect(pgOut.file?.content).toContain('Database="postgres"');
    expect(pgOut.file?.content).not.toContain("s3cret+1");
  });

  it("generates official pgAdmin servers.json without password", () => {
    const out = generateClientOutput("pgadmin", pg, "pg-branch", { includePassword: true });
    expect(out.file?.filename).toBe("pg-branch-pgadmin-servers.json");
    const parsed = JSON.parse(out.file!.content);
    expect(parsed).toEqual({
      Servers: {
        "1": {
          Name: "pg-branch",
          Group: "db-branch",
          Host: "192.0.2.11",
          Port: 40002,
          MaintenanceDB: "postgres",
          Username: "alice-feat",
          SSLMode: "prefer",
        },
      },
    });
  });

  it("generates MySQL Workbench connections.xml without password", () => {
    const out = generateClientOutput("workbench", mysql, "my-branch", { viaProxy: true });
    expect(out.file?.filename).toBe("my-branch-proxysql-workbench-connections.xml");
    expect(out.file?.content).toContain('<data grt_format="2.0">');
    expect(out.file?.content).toContain('struct-name="db.mgmt.Connection"');
    expect(out.file?.content).toContain('<value type="string" key="hostName">192.0.2.10</value>');
    expect(out.file?.content).toContain('<value type="int" key="port">40001</value>');
    expect(out.file?.content).toContain('<value type="string" key="userName">alice-feat</value>');
    expect(out.file?.content).toContain('<value type="string" key="name">my-branch (ProxySQL)</value>');
  });

  it("generates TablePlus URL with optional password", () => {
    expect(generateClientOutput("tableplus", mysql, "my-branch").clipboard?.text).toBe("mysql://alice-feat@192.0.2.10:40001/");
    expect(generateClientOutput("tableplus", mysql, "my-branch", { includePassword: true }).clipboard?.text).toBe(
      "mysql://alice-feat:p%40ss%26w%3C0%3Erd@192.0.2.10:40001/",
    );
    expect(generateClientOutput("tableplus", pg, "pg-branch", { includePassword: true, maskPassword: true }).clipboard?.text).toBe(
      "postgres://alice-feat:••••••@192.0.2.11:40002/postgres",
    );
  });

  it("sanitizes leading dots and special characters in filenames", () => {
    expect(sanitizeFilename("..")).toBe("branch");
    expect(sanitizeFilename("...foo/bar")).toBe("foo-bar");
  });

  it("URL-encodes database names in JDBC URLs to prevent query parameter injection", () => {
    const specialDb = connectionFrom("mysql://alice:pw@192.0.2.10:40001/app%3FallowLoadLocalInfile%3Dtrue");
    const out = generateClientOutput("datagrip", specialDb, "alice-special");
    expect(out.clipboard?.text).toBe("jdbc:mysql://192.0.2.10:40001/app%3FallowLoadLocalInfile%3Dtrue?user=alice");
    expect(out.file?.content).toContain("<jdbc-url>jdbc:mysql://192.0.2.10:40001/app%3FallowLoadLocalInfile%3Dtrue</jdbc-url>");
  });

  it("provides Japanese translations for every client hintKey", () => {
    for (const spec of CLIENT_SPECS) {
      expect(ja[spec.hintKey]).toBeTruthy();
    }
  });

  it("generates standard deterministic UUID v5 identifiers and strips XML control characters", () => {
    const out1 = generateClientOutput("datagrip", mysql, "alice-feat");
    const out2 = generateClientOutput("datagrip", mysql, "alice-feat");
    const uuidMatch = out1.file?.content.match(/uuid="([^"]+)"/);
    expect(uuidMatch?.[1]).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    expect(out1.file?.content).toBe(out2.file?.content);

    const wb = generateClientOutput("workbench", mysql, "alice-feat");
    const wbId = wb.file?.content.match(/id="([^"]+)"/);
    expect(wbId?.[1]).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);

    const ctrlConn = connectionFrom("mysql://u%00ser:pw@192.0.2.10:40001/db%07name");
    const navOut = generateClientOutput("navicat", ctrlConn, "br\x1bname");
    expect(navOut.file?.content).not.toMatch(/[\x00-\x08\x0b\x0c\x0e-\x1f]/);

    // Cross-language RFC 4122 UUID v5 vector matching Go uuid.NewSHA1(uuid.NameSpaceURL, ...).
    const vecConn = connectionFrom("mysql://alice:pw@192.0.2.10:40001/");
    const vecOut = generateClientOutput("datagrip", vecConn, "alice");
    expect(vecOut.file?.content).toContain('uuid="070efc39-c71a-54ec-b5c7-5c66660f4b2e"');
  });
});
