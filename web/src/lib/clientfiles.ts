import type { Connection } from "./connect";

export type ClientId = "dbeaver" | "datagrip" | "navicat" | "pgadmin" | "workbench" | "tableplus";
export type PasswordMode = "plaintext" | "none";

export type ClientSpec = {
  id: ClientId;
  label: string;
  supportsFile: (engine: string) => boolean;
  supportsClipboard: (engine: string) => boolean;
  filePasswordMode: PasswordMode | null;
  clipboardPasswordMode: PasswordMode | null;
  hintKey: string;
};

export type ClientOutputOptions = {
  includePassword?: boolean;
  maskPassword?: boolean;
  viaProxy?: boolean;
};

export type ClientOutput = {
  spec: ClientSpec;
  file?: {
    filename: string;
    mime: string;
    content: string;
    passwordMode: PasswordMode;
  };
  clipboard?: {
    labelKey: string;
    text: string;
    passwordMode: PasswordMode;
  };
};

const MASK = "••••••";

function normalizeEngine(engine: string): string {
  const e = engine.toLowerCase();
  return e === "postgresql" ? "postgres" : e;
}

function formatHostPort(host: string, port: number): string {
  const h = host.includes(":") && !host.startsWith("[") ? `[${host}]` : host;
  return `${h}:${port}`;
}

export function sanitizeFilename(name: string): string {
  const cleaned = name.replace(/[^A-Za-z0-9._-]/g, "-").replace(/^\.+/, "");
  return cleaned || "branch";
}

function xmlEscape(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&apos;");
}

// Deterministic UUID v4-shaped identifier so generated XML files remain stable across renders/tests.
function deterministicUuid(seed: string): string {
  const words = [0x811c9dc5, 0x01000193, 0x9e3779b9, 0x85ebca6b];
  for (let i = 0; i < seed.length; i++) {
    const c = seed.charCodeAt(i);
    for (let j = 0; j < words.length; j++) {
      words[j] = Math.imul(words[j] ^ (c + j * 31), 0x01000193) >>> 0;
    }
  }
  const hex = words.map((w) => w.toString(16).padStart(8, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-4${hex.slice(13, 16)}-8${hex.slice(17, 20)}-${hex.slice(20, 32)}`;
}

// connectionForProxy adapts a direct branch Connection to target its ProxySQL endpoint.
export function connectionForProxy(conn: Connection, proxy: { host: string; port: number; user: string }): Connection {
  const hostPart = formatHostPort(proxy.host, proxy.port);
  const auth = `${encodeURIComponent(proxy.user)}:${encodeURIComponent(conn.password)}`;
  const dbPath = conn.database ? encodeURIComponent(conn.database) : "";
  return {
    ...conn,
    host: proxy.host,
    port: proxy.port,
    user: proxy.user,
    url: `${conn.engine}://${auth}@${hostPart}/${dbPath}`,
  };
}

function jdbcSchemeFor(engine: string): string {
  const norm = normalizeEngine(engine);
  return norm === "postgres" ? "postgresql" : engine.toLowerCase();
}

function jdbcBaseUrl(conn: Connection): string {
  const dbPart = conn.database ? encodeURIComponent(conn.database) : "";
  return `jdbc:${jdbcSchemeFor(conn.engine)}://${formatHostPort(conn.host, conn.port)}/${dbPart}`;
}

function displayName(branch: string, viaProxy?: boolean): string {
  return viaProxy ? `${branch} (ProxySQL)` : branch;
}

function fileStem(branch: string, viaProxy?: boolean): string {
  return `${sanitizeFilename(branch)}${viaProxy ? "-proxysql" : ""}`;
}

function effectivePassword(conn: Connection, opts: ClientOutputOptions): string | undefined {
  if (!opts.includePassword) return undefined;
  return opts.maskPassword ? MASK : conn.password;
}

const DBEAVER_DRIVERS: Record<string, { provider: string; driver: string }> = {
  mysql: { provider: "mysql", driver: "mysql8" },
  postgres: { provider: "postgresql", driver: "postgres-jdbc" },
  mariadb: { provider: "mysql", driver: "mariaDB" },
};

const DATAGRIP_DRIVERS: Record<string, { driverRef: string; jdbcDriver: string }> = {
  mysql: { driverRef: "mysql.8", jdbcDriver: "com.mysql.cj.jdbc.Driver" },
  postgres: { driverRef: "postgresql", jdbcDriver: "org.postgresql.Driver" },
  mariadb: { driverRef: "mariadb", jdbcDriver: "org.mariadb.jdbc.Driver" },
};

const NAVICAT_TYPES: Record<string, string> = {
  mysql: "MYSQL",
  postgres: "POSTGRESQL",
  mariadb: "MARIADB",
};

const WORKBENCH_ENGINES = new Set(["mysql", "mariadb"]);

export const CLIENT_SPECS: readonly ClientSpec[] = [
  {
    id: "dbeaver",
    label: "DBeaver (data-sources.json)",
    supportsFile: (e) => normalizeEngine(e) in DBEAVER_DRIVERS,
    supportsClipboard: () => false,
    filePasswordMode: "plaintext",
    clipboardPasswordMode: null,
    hintKey: "Place in your DBeaver workspace (.dbeaver/data-sources*.json) or import via Project.",
  },
  {
    id: "datagrip",
    label: "DataGrip / JetBrains (dataSources.xml)",
    supportsFile: (e) => normalizeEngine(e) in DATAGRIP_DRIVERS,
    supportsClipboard: () => true,
    filePasswordMode: "none",
    clipboardPasswordMode: "plaintext",
    hintKey: "Save or merge into .idea/dataSources.xml in your project, or copy the JDBC URL for Data Source from URL.",
  },
  {
    id: "navicat",
    label: "Navicat / Chat2DB (.ncx)",
    supportsFile: (e) => normalizeEngine(e) in NAVICAT_TYPES,
    supportsClipboard: () => false,
    filePasswordMode: "none",
    clipboardPasswordMode: null,
    hintKey: "Choose File → Import Connections in Navicat (also supported by DBeaver and Chat2DB).",
  },
  {
    id: "pgadmin",
    label: "pgAdmin (servers.json)",
    supportsFile: (e) => normalizeEngine(e) === "postgres",
    supportsClipboard: () => false,
    filePasswordMode: "none",
    clipboardPasswordMode: null,
    hintKey: "Open Tools → Import/Export Servers in pgAdmin and select this file.",
  },
  {
    id: "workbench",
    label: "MySQL Workbench (connections.xml)",
    supportsFile: (e) => WORKBENCH_ENGINES.has(normalizeEngine(e)),
    supportsClipboard: () => false,
    filePasswordMode: "none",
    clipboardPasswordMode: null,
    hintKey: "Rename to connections.xml in your MySQL Workbench config folder (merge the Connection block if one already exists).",
  },
  {
    id: "tableplus",
    label: "TablePlus (URL)",
    supportsFile: () => false,
    supportsClipboard: () => true,
    filePasswordMode: null,
    clipboardPasswordMode: "plaintext",
    hintKey: "Copy the URL and choose Import Connection → From URL in TablePlus.",
  },
];

export function clientSpecsFor(engine: string): ClientSpec[] {
  return CLIENT_SPECS.filter((s) => s.supportsFile(engine) || s.supportsClipboard(engine));
}

function buildDBeaverJson(conn: Connection, branch: string, opts: ClientOutputOptions): string {
  const drv = DBEAVER_DRIVERS[normalizeEngine(conn.engine)];
  const pw = effectivePassword(conn, opts);
  const name = displayName(branch, opts.viaProxy);
  const id = `db-branch-${fileStem(branch, opts.viaProxy)}`;
  const config: Record<string, unknown> = {
    host: conn.host,
    port: String(conn.port),
    database: conn.database,
    url: jdbcBaseUrl(conn),
    configurationType: "MANUAL",
    type: "dev",
    "auth-model": "native",
    user: conn.user,
  };
  if (pw !== undefined) config.password = pw;

  return `${JSON.stringify(
    {
      folders: {},
      connections: {
        [id]: {
          provider: drv.provider,
          driver: drv.driver,
          name,
          "save-password": pw !== undefined,
          configuration: config,
        },
      },
    },
    null,
    2,
  )}\n`;
}

function buildDataGripXml(conn: Connection, branch: string, opts: ClientOutputOptions): string {
  const drv = DATAGRIP_DRIVERS[normalizeEngine(conn.engine)];
  const name = displayName(branch, opts.viaProxy);
  const uuid = deterministicUuid(`datagrip:${name}:${conn.host}:${conn.port}:${conn.user}`);
  return [
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<project version="4">',
    '  <component name="DataSourceManagerImpl" format="xml" multifile-model="true">',
    `    <data-source source="LOCAL" name="${xmlEscape(name)}" uuid="${uuid}">`,
    `      <driver-ref>${drv.driverRef}</driver-ref>`,
    "      <synchronize>true</synchronize>",
    `      <jdbc-driver>${drv.jdbcDriver}</jdbc-driver>`,
    `      <jdbc-url>${xmlEscape(jdbcBaseUrl(conn))}</jdbc-url>`,
    `      <user-name>${xmlEscape(conn.user)}</user-name>`,
    "      <working-dir>$ProjectFileDir$</working-dir>",
    "    </data-source>",
    "  </component>",
    "</project>",
    "",
  ].join("\n");
}

function buildJdbcUrl(conn: Connection, opts: ClientOutputOptions): string {
  const base = jdbcBaseUrl(conn);
  const pw = effectivePassword(conn, opts);
  const params = [`user=${encodeURIComponent(conn.user)}`];
  if (pw !== undefined) {
    params.push(`password=${opts.maskPassword ? MASK : encodeURIComponent(pw)}`);
  }
  return `${base}?${params.join("&")}`;
}

function buildNavicatNcx(conn: Connection, branch: string, opts: ClientOutputOptions): string {
  const connType = NAVICAT_TYPES[normalizeEngine(conn.engine)];
  const name = displayName(branch, opts.viaProxy);
  return [
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<Connections Ver="1.5">',
    `  <Connection ConnectionName="${xmlEscape(name)}" ConnType="${connType}" Host="${xmlEscape(conn.host)}" Port="${conn.port}" Database="${xmlEscape(conn.database)}" UserName="${xmlEscape(conn.user)}" SavePassword="false"/>`,
    "</Connections>",
    "",
  ].join("\n");
}

function buildPgAdminJson(conn: Connection, branch: string, opts: ClientOutputOptions): string {
  return `${JSON.stringify(
    {
      Servers: {
        "1": {
          Name: displayName(branch, opts.viaProxy),
          Group: "db-branch",
          Host: conn.host,
          Port: conn.port,
          MaintenanceDB: conn.database || "postgres",
          Username: conn.user,
          SSLMode: "prefer",
        },
      },
    },
    null,
    2,
  )}\n`;
}

function buildWorkbenchXml(conn: Connection, branch: string, opts: ClientOutputOptions): string {
  const name = displayName(branch, opts.viaProxy);
  const id = deterministicUuid(`workbench:${name}:${conn.host}:${conn.port}:${conn.user}`);
  return [
    '<?xml version="1.0"?>',
    '<data grt_format="2.0">',
    '  <value type="list" content-type="object" content-struct-name="db.mgmt.Connection">',
    `    <value type="object" struct-name="db.mgmt.Connection" id="${id}" struct-checksum="0x96ba47d8">`,
    '      <link type="object" struct-name="db.mgmt.Driver" key="driver">com.mysql.rdbms.mysql.driver.native</link>',
    `      <value type="string" key="hostIdentifier">${xmlEscape(`Mysql@${formatHostPort(conn.host, conn.port)}`)}</value>`,
    '      <value type="int" key="isDefault">0</value>',
    '      <value type="dict" key="modules"/>',
    '      <value type="dict" key="parameterValues">',
    `        <value type="string" key="defaultSchema">${xmlEscape(conn.database)}</value>`,
    `        <value type="string" key="hostName">${xmlEscape(conn.host)}</value>`,
    `        <value type="string" key="name">${xmlEscape(name)}</value>`,
    `        <value type="int" key="port">${conn.port}</value>`,
    `        <value type="string" key="userName">${xmlEscape(conn.user)}</value>`,
    '        <value type="string" key="useSSL">IF_AVAILABLE</value>',
    "      </value>",
    '      <link type="object" struct-name="db.mgmt.ServerInstance" key="serverInstance"/>',
    "    </value>",
    "  </value>",
    "</data>",
    "",
  ].join("\n");
}

function buildTablePlusUrl(conn: Connection, opts: ClientOutputOptions): string {
  const pw = effectivePassword(conn, opts);
  const pwPart = pw === undefined ? "" : `:${opts.maskPassword ? MASK : encodeURIComponent(pw)}`;
  const dbPart = conn.database ? encodeURIComponent(conn.database) : "";
  return `${conn.engine}://${encodeURIComponent(conn.user)}${pwPart}@${formatHostPort(conn.host, conn.port)}/${dbPart}`;
}

export function generateClientOutput(id: ClientId, conn: Connection, branch: string, opts: ClientOutputOptions = {}): ClientOutput {
  const spec = CLIENT_SPECS.find((s) => s.id === id) ?? CLIENT_SPECS[0];
  const stem = fileStem(branch, opts.viaProxy);

  switch (spec.id) {
    case "dbeaver":
      return {
        spec,
        file: spec.supportsFile(conn.engine)
          ? { filename: `data-sources-${stem}.json`, mime: "application/json;charset=utf-8", content: buildDBeaverJson(conn, branch, opts), passwordMode: "plaintext" }
          : undefined,
      };
    case "datagrip":
      return {
        spec,
        file: spec.supportsFile(conn.engine)
          ? { filename: `dataSources-${stem}.xml`, mime: "application/xml;charset=utf-8", content: buildDataGripXml(conn, branch, opts), passwordMode: "none" }
          : undefined,
        clipboard: { labelKey: "Copy JDBC URL", text: buildJdbcUrl(conn, opts), passwordMode: "plaintext" },
      };
    case "navicat":
      return {
        spec,
        file: spec.supportsFile(conn.engine)
          ? { filename: `${stem}.ncx`, mime: "application/xml;charset=utf-8", content: buildNavicatNcx(conn, branch, opts), passwordMode: "none" }
          : undefined,
      };
    case "pgadmin":
      return {
        spec,
        file: spec.supportsFile(conn.engine)
          ? { filename: `${stem}-pgadmin-servers.json`, mime: "application/json;charset=utf-8", content: buildPgAdminJson(conn, branch, opts), passwordMode: "none" }
          : undefined,
      };
    case "workbench":
      return {
        spec,
        file: spec.supportsFile(conn.engine)
          ? { filename: `${stem}-workbench-connections.xml`, mime: "application/xml;charset=utf-8", content: buildWorkbenchXml(conn, branch, opts), passwordMode: "none" }
          : undefined,
      };
    case "tableplus":
      return {
        spec,
        clipboard: { labelKey: "Copy URL", text: buildTablePlusUrl(conn, opts), passwordMode: "plaintext" },
      };
  }
}
