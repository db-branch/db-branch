import type { Connection } from "./connect";
import { buildDBeaverDbp, xmlEscape } from "./dbeaverdbp";

export type ClientId = "dbeaver" | "chat2db" | "datagrip" | "navicat" | "pgadmin" | "workbench" | "tableplus";
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
    bytes?: Uint8Array;
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

// RFC 4122 NameSpace_URL (6ba7b811-9dad-11d1-80b4-00c04fd430c8).
const UUID_NAMESPACE_URL = new Uint8Array([0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8]);

function rotl32(v: number, n: number): number {
  return ((v << n) | (v >>> (32 - n))) >>> 0;
}

function sha1(data: Uint8Array): Uint8Array {
  const bitLen = data.length * 8;
  const paddedLen = Math.ceil((data.length + 9) / 64) * 64;
  const msg = new Uint8Array(paddedLen);
  msg.set(data);
  msg[data.length] = 0x80;
  const dv = new DataView(msg.buffer);
  dv.setUint32(paddedLen - 8, Math.floor(bitLen / 0x100000000), false);
  dv.setUint32(paddedLen - 4, bitLen >>> 0, false);

  let h0 = 0x67452301;
  let h1 = 0xefcdab89;
  let h2 = 0x98badcfe;
  let h3 = 0x10325476;
  let h4 = 0xc3d2e1f0;
  const w = new Uint32Array(80);

  for (let off = 0; off < paddedLen; off += 64) {
    for (let i = 0; i < 16; i++) w[i] = dv.getUint32(off + i * 4, false);
    for (let i = 16; i < 80; i++) w[i] = rotl32(w[i - 3] ^ w[i - 8] ^ w[i - 14] ^ w[i - 16], 1);

    let a = h0, b = h1, c = h2, d = h3, e = h4;
    for (let i = 0; i < 80; i++) {
      let f: number, k: number;
      if (i < 20) {
        f = (b & c) | (~b & d);
        k = 0x5a827999;
      } else if (i < 40) {
        f = b ^ c ^ d;
        k = 0x6ed9eba1;
      } else if (i < 60) {
        f = (b & c) | (b & d) | (c & d);
        k = 0x8f1bbcdc;
      } else {
        f = b ^ c ^ d;
        k = 0xca62c1d6;
      }
      const temp = (rotl32(a, 5) + f + e + k + w[i]) >>> 0;
      e = d;
      d = c;
      c = rotl32(b, 30);
      b = a;
      a = temp;
    }
    h0 = (h0 + a) >>> 0;
    h1 = (h1 + b) >>> 0;
    h2 = (h2 + c) >>> 0;
    h3 = (h3 + d) >>> 0;
    h4 = (h4 + e) >>> 0;
  }

  const out = new Uint8Array(20);
  const ov = new DataView(out.buffer);
  ov.setUint32(0, h0, false);
  ov.setUint32(4, h1, false);
  ov.setUint32(8, h2, false);
  ov.setUint32(12, h3, false);
  ov.setUint32(16, h4, false);
  return out;
}

// Deterministic RFC 4122 UUID v5 (SHA-1) matching Go's uuid.NewSHA1(uuid.NameSpaceURL, seed).
function deterministicUuid(seed: string): string {
  const seedBytes = new TextEncoder().encode(seed);
  const input = new Uint8Array(UUID_NAMESPACE_URL.length + seedBytes.length);
  input.set(UUID_NAMESPACE_URL, 0);
  input.set(seedBytes, UUID_NAMESPACE_URL.length);
  const hash = sha1(input).subarray(0, 16);
  hash[6] = (hash[6] & 0x0f) | 0x50;
  hash[8] = (hash[8] & 0x3f) | 0x80;
  const hex = Array.from(hash, (b) => b.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20, 32)}`;
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

const CHAT2DB_TYPES: Record<string, string> = {
  mysql: "MYSQL",
  postgres: "POSTGRESQL",
  mariadb: "MARIADB",
  clickhouse: "CLICKHOUSE",
  sqlite: "SQLITE",
  oracle: "ORACLE",
  sqlserver: "SQLSERVER",
  mongodb: "MONGODB",
};

export const CLIENT_SPECS: readonly ClientSpec[] = [
  {
    id: "dbeaver",
    label: "DBeaver (.dbp)",
    supportsFile: (e) => normalizeEngine(e) in DBEAVER_DRIVERS,
    supportsClipboard: () => false,
    filePasswordMode: "plaintext",
    clipboardPasswordMode: null,
    hintKey: "Import via File → Import → DBeaver → Project in DBeaver (also supported by Chat2DB DBeaver import).",
  },
  {
    id: "chat2db",
    label: "Chat2DB (.json)",
    supportsFile: (e) => normalizeEngine(e) in CHAT2DB_TYPES,
    supportsClipboard: () => false,
    filePasswordMode: "none",
    clipboardPasswordMode: null,
    hintKey: "Import via Import Connections → Chat2DB in Chat2DB and select this .json file.",
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
    label: "Navicat (.ncx)",
    supportsFile: (e) => normalizeEngine(e) in NAVICAT_TYPES,
    supportsClipboard: () => false,
    filePasswordMode: "none",
    clipboardPasswordMode: null,
    hintKey: "Choose File → Import Connections in Navicat (also supported by DBeaver and Chat2DB Navicat import).",
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

function buildDBeaverJson(conn: Connection, branch: string, opts: ClientOutputOptions, inlinePassword = true): string {
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
  if (inlinePassword && pw !== undefined) config.password = pw;

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

function buildChat2DBJson(conn: Connection, branch: string, opts: ClientOutputOptions): string {
  const type = CHAT2DB_TYPES[normalizeEngine(conn.engine)];
  return `${JSON.stringify(
    [
      {
        alias: displayName(branch, opts.viaProxy),
        type,
        host: conn.host,
        port: String(conn.port),
        url: jdbcBaseUrl(conn),
        user: conn.user,
        ssh: { use: false },
      },
    ],
    null,
    2,
  )}\n`;
}

function buildNavicatNcx(conn: Connection, branch: string, opts: ClientOutputOptions): string {
  const connType = NAVICAT_TYPES[normalizeEngine(conn.engine)];
  const name = displayName(branch, opts.viaProxy);
  return [
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<Connections Ver="1.5">',
    `  <Connection ConnectionName="${xmlEscape(name)}" ConnType="${connType}" Host="${xmlEscape(conn.host)}" Port="${conn.port}" Database="${xmlEscape(conn.database)}" UserName="${xmlEscape(conn.user)}" SSH="false" SavePassword="false"/>`,
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
    case "dbeaver": {
      if (!spec.supportsFile(conn.engine)) return { spec };
      const previewJson = buildDBeaverJson(conn, branch, opts);
      // .dbp carries credentials-config.json (AES-128-CBC with DBeaver's public local key) and clean data-sources.json
      // (save-password matches includePassword while keeping configuration.password out of data-sources.json).
      const archiveJson = buildDBeaverJson(conn, branch, { ...opts, maskPassword: false }, false);
      const pw = effectivePassword(conn, opts);
      const bytes = opts.maskPassword
        ? undefined
        : buildDBeaverDbp({
            projectName: `db-branch-${stem}`,
            connectionId: `db-branch-${stem}`,
            dataSourcesJson: archiveJson,
            user: conn.user,
            password: pw,
          });
      return {
        spec,
        file: {
          filename: `${stem}.dbp`,
          mime: "application/zip",
          content: previewJson,
          bytes,
          passwordMode: "plaintext",
        },
      };
    }
    case "chat2db":
      return {
        spec,
        file: spec.supportsFile(conn.engine)
          ? { filename: `${stem}-chat2db.json`, mime: "application/json;charset=utf-8", content: buildChat2DBJson(conn, branch, opts), passwordMode: "none" }
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
