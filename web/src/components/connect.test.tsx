import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it } from "vitest";
import { connectionFrom } from "../lib/connect";
import { setLanguage } from "../lib/i18n";
import { ClientFilePanel } from "./connect";

const mysqlConn = connectionFrom("mysql://alice-feat:p%40ss@192.0.2.10:40001/");
const pgConn = connectionFrom("postgres://alice-feat:s3cret@192.0.2.11:40002/postgres");
const proxy = { host: "proxy.example.com", port: 6033, user: "alice-feat" };

afterEach(() => setLanguage("en"));

describe("ClientFilePanel", () => {
  it("renders client options, download action, and omits passwords by default", () => {
    const html = renderToStaticMarkup(<ClientFilePanel branch="alice-feat" conn={mysqlConn} proxy={proxy} reveal={false} />);
    expect(html).toContain("DBeaver (.dbp)");
    expect(html).toContain("Chat2DB (.json)");
    expect(html).toContain("DataGrip / JetBrains (dataSources.xml)");
    expect(html).toContain("Navicat (.ncx)");
    expect(html).toContain("MySQL Workbench (connections.xml)");
    expect(html).toContain("TablePlus (URL)");
    expect(html).not.toContain("pgAdmin (servers.json)");
    expect(html).toContain("Direct");
    expect(html).toContain("ProxySQL");
    expect(html).toContain("Include password");
    expect(html).toContain("Download");
    expect(html).not.toContain("p@ss");
  });

  it("shows pgAdmin and hides ProxySQL target for PostgreSQL", () => {
    const html = renderToStaticMarkup(<ClientFilePanel branch="pg-feat" conn={pgConn} proxy={proxy} reveal={false} />);
    expect(html).toContain("pgAdmin (servers.json)");
    expect(html).not.toContain("MySQL Workbench (connections.xml)");
    expect(html).not.toContain("ProxySQL");
    expect(html).not.toContain("s3cret");
  });

  it("renders Japanese instructions and warnings when language is ja", () => {
    setLanguage("ja");
    const html = renderToStaticMarkup(<ClientFilePanel branch="alice-feat" conn={mysqlConn} proxy={null} reveal={false} />);
    expect(html).toContain("平文で保存されます");
    expect(html).toContain("DBeaver");
  });
});
