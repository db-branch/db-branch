// Minimal synchronous PKZIP (store-only) + DBeaver AES-128-CBC credentials
// builder so .dbp archives can be generated in the browser without external
// dependencies and imported by both DBeaver and Chat2DB.
import { LocalizedError } from "./errors";
import { t } from "./i18n";

const DBEAVER_LOCAL_KEY = new Uint8Array([
  0xba, 0xbb, 0x4a, 0x9f, 0x77, 0x4a, 0xb8, 0x53,
  0xc9, 0x6c, 0x2d, 0x65, 0x3d, 0xfe, 0x54, 0x4a,
]);

// Standard AES S-box (FIPS 197).
const SBOX = new Uint8Array([
  0x63, 0x7c, 0x77, 0x7b, 0xf2, 0x6b, 0x6f, 0xc5, 0x30, 0x01, 0x67, 0x2b, 0xfe, 0xd7, 0xab, 0x76,
  0xca, 0x82, 0xc9, 0x7d, 0xfa, 0x59, 0x47, 0xf0, 0xad, 0xd4, 0xa2, 0xaf, 0x9c, 0xa4, 0x72, 0xc0,
  0xb7, 0xfd, 0x93, 0x26, 0x36, 0x3f, 0xf7, 0xcc, 0x34, 0xa5, 0xe5, 0xf1, 0x71, 0xd8, 0x31, 0x15,
  0x04, 0xc7, 0x23, 0xc3, 0x18, 0x96, 0x05, 0x9a, 0x07, 0x12, 0x80, 0xe2, 0xeb, 0x27, 0xb2, 0x75,
  0x09, 0x83, 0x2c, 0x1a, 0x1b, 0x6e, 0x5a, 0xa0, 0x52, 0x3b, 0xd6, 0xb3, 0x29, 0xe3, 0x2f, 0x84,
  0x53, 0xd1, 0x00, 0xed, 0x20, 0xfc, 0xb1, 0x5b, 0x6a, 0xcb, 0xbe, 0x39, 0x4a, 0x4c, 0x58, 0xcf,
  0xd0, 0xef, 0xaa, 0xfb, 0x43, 0x4d, 0x33, 0x85, 0x45, 0xf9, 0x02, 0x7f, 0x50, 0x3c, 0x9f, 0xa8,
  0x51, 0xa3, 0x40, 0x8f, 0x92, 0x9d, 0x38, 0xf5, 0xbc, 0xb6, 0xda, 0x21, 0x10, 0xff, 0xf3, 0xd2,
  0xcd, 0x0c, 0x13, 0xec, 0x5f, 0x97, 0x44, 0x17, 0xc4, 0xa7, 0x7e, 0x3d, 0x64, 0x5d, 0x19, 0x73,
  0x60, 0x81, 0x4f, 0xdc, 0x22, 0x2a, 0x90, 0x88, 0x46, 0xee, 0xb8, 0x14, 0xde, 0x5e, 0x0b, 0xdb,
  0xe0, 0x32, 0x3a, 0x0a, 0x49, 0x06, 0x24, 0x5c, 0xc2, 0xd3, 0xac, 0x62, 0x91, 0x95, 0xe4, 0x79,
  0xe7, 0xc8, 0x37, 0x6d, 0x8d, 0xd5, 0x4e, 0xa9, 0x6c, 0x56, 0xf4, 0xea, 0x65, 0x7a, 0xae, 0x08,
  0xba, 0x78, 0x25, 0x2e, 0x1c, 0xa6, 0xb4, 0xc6, 0xe8, 0xdd, 0x74, 0x1f, 0x4b, 0xbd, 0x8b, 0x8a,
  0x70, 0x3e, 0xb5, 0x66, 0x48, 0x03, 0xf6, 0x0e, 0x61, 0x35, 0x57, 0xb9, 0x86, 0xc1, 0x1d, 0x9e,
  0xe1, 0xf8, 0x98, 0x11, 0x69, 0xd9, 0x8e, 0x94, 0x9b, 0x1e, 0x87, 0xe9, 0xce, 0x55, 0x28, 0xdf,
  0x8c, 0xa1, 0x89, 0x0d, 0xbf, 0xe6, 0x42, 0x68, 0x41, 0x99, 0x2d, 0x0f, 0xb0, 0x54, 0xbb, 0x16,
]);

const RCON = [0x01, 0x02, 0x04, 0x08, 0x10, 0x20, 0x40, 0x80, 0x1b, 0x36];

function xtime(a: number): number {
  return ((a << 1) ^ (a & 0x80 ? 0x1b : 0)) & 0xff;
}

function expandKey128(key: Uint8Array): Uint8Array {
  const rk = new Uint8Array(176);
  rk.set(key);
  for (let i = 16, r = 0; i < 176; i += 4) {
    let t0 = rk[i - 4], t1 = rk[i - 3], t2 = rk[i - 2], t3 = rk[i - 1];
    if (i % 16 === 0) {
      [t0, t1, t2, t3] = [SBOX[t1] ^ RCON[r++], SBOX[t2], SBOX[t3], SBOX[t0]];
    }
    rk[i] = rk[i - 16] ^ t0;
    rk[i + 1] = rk[i - 15] ^ t1;
    rk[i + 2] = rk[i - 14] ^ t2;
    rk[i + 3] = rk[i - 13] ^ t3;
  }
  return rk;
}

function aes128EncryptBlock(block: Uint8Array, rk: Uint8Array, out: Uint8Array, outOff: number): void {
  const s = new Uint8Array(16);
  for (let i = 0; i < 16; i++) s[i] = block[i] ^ rk[i];
  for (let round = 1; round <= 10; round++) {
    for (let i = 0; i < 16; i++) s[i] = SBOX[s[i]];
    // ShiftRows (column-major state)
    const t1 = s[1]; s[1] = s[5]; s[5] = s[9]; s[9] = s[13]; s[13] = t1;
    const t2 = s[2]; s[2] = s[10]; s[10] = t2;
    const t6 = s[6]; s[6] = s[14]; s[14] = t6;
    const t15 = s[15]; s[15] = s[11]; s[11] = s[7]; s[7] = s[3]; s[3] = t15;
    if (round < 10) {
      for (let c = 0; c < 16; c += 4) {
        const a0 = s[c], a1 = s[c + 1], a2 = s[c + 2], a3 = s[c + 3];
        const u = a0 ^ a1 ^ a2 ^ a3;
        s[c] ^= u ^ xtime(a0 ^ a1);
        s[c + 1] ^= u ^ xtime(a1 ^ a2);
        s[c + 2] ^= u ^ xtime(a2 ^ a3);
        s[c + 3] ^= u ^ xtime(a3 ^ a0);
      }
    }
    const rOff = round * 16;
    for (let i = 0; i < 16; i++) s[i] ^= rk[rOff + i];
  }
  out.set(s, outOff);
}

function randomIv(): Uint8Array {
  if (typeof globalThis.crypto?.getRandomValues !== "function") {
    throw new LocalizedError(t("Random number generation is unavailable in this browser; cannot encrypt credentials."));
  }
  const iv = new Uint8Array(16);
  globalThis.crypto.getRandomValues(iv);
  return iv;
}

export function encryptDBeaverCredentials(plaintextJson: string, iv: Uint8Array = randomIv()): Uint8Array {
  const raw = new TextEncoder().encode(plaintextJson);
  const pad = 16 - (raw.length % 16);
  const padded = new Uint8Array(raw.length + pad);
  padded.set(raw);
  padded.fill(pad, raw.length);

  const rk = expandKey128(DBEAVER_LOCAL_KEY);
  const out = new Uint8Array(16 + padded.length);
  out.set(iv, 0);
  const block = new Uint8Array(16);
  let prev = iv;
  for (let off = 0; off < padded.length; off += 16) {
    for (let i = 0; i < 16; i++) block[i] = padded[off + i] ^ prev[i];
    aes128EncryptBlock(block, rk, out, 16 + off);
    prev = out.subarray(16 + off, 32 + off);
  }
  return out;
}

export function xmlEscape(s: string): string {
  return s
    // Replace characters forbidden by XML 1.0 (0x00-0x08, 0x0B-0x0C, 0x0E-0x1F, 0xFFFE-0xFFFF) with U+FFFD, matching Go encoding/xml.
    .replace(/[\x00-\x08\x0b\x0c\x0e-\x1f\ufffe\uffff]/g, "\ufffd")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&apos;")
    .replace(/\t/g, "&#x9;")
    .replace(/\n/g, "&#xA;")
    .replace(/\r/g, "&#xD;");
}

const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let i = 0; i < 256; i++) {
    let c = i;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[i] = c >>> 0;
  }
  return t;
})();

function crc32(data: Uint8Array): number {
  let c = 0xffffffff;
  for (let i = 0; i < data.length; i++) c = CRC_TABLE[(c ^ data[i]) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

export type ZipEntryInput = { name: string; data: Uint8Array; isDir?: boolean };

export function buildZip(entries: ZipEntryInput[]): Uint8Array {
  const enc = new TextEncoder();
  const localParts: Uint8Array[] = [];
  const centralParts: Uint8Array[] = [];
  let offset = 0;

  for (const e of entries) {
    const nameBytes = enc.encode(e.name);
    const data = e.isDir ? new Uint8Array(0) : e.data;
    const crc = crc32(data);

    const local = new Uint8Array(30 + nameBytes.length);
    const lv = new DataView(local.buffer);
    lv.setUint32(0, 0x04034b50, true);
    lv.setUint16(4, 20, true);
    lv.setUint16(6, 0x0800, true); // UTF-8 filename
    lv.setUint16(8, 0, true); // store (no compression)
    lv.setUint32(14, crc, true);
    lv.setUint32(18, data.length, true);
    lv.setUint32(22, data.length, true);
    lv.setUint16(26, nameBytes.length, true);
    local.set(nameBytes, 30);
    localParts.push(local, data);

    const central = new Uint8Array(46 + nameBytes.length);
    const cv = new DataView(central.buffer);
    cv.setUint32(0, 0x02014b50, true);
    cv.setUint16(4, 0x031e, true);
    cv.setUint16(6, 20, true);
    cv.setUint16(8, 0x0800, true);
    cv.setUint16(10, 0, true);
    cv.setUint32(16, crc, true);
    cv.setUint32(20, data.length, true);
    cv.setUint32(24, data.length, true);
    cv.setUint16(28, nameBytes.length, true);
    cv.setUint32(38, e.isDir ? 0x10 : 0, true);
    cv.setUint32(42, offset, true);
    central.set(nameBytes, 46);
    centralParts.push(central);

    offset += local.length + data.length;
  }

  const cdSize = centralParts.reduce((a, b) => a + b.length, 0);
  const eocd = new Uint8Array(22);
  const ev = new DataView(eocd.buffer);
  ev.setUint32(0, 0x06054b50, true);
  ev.setUint16(8, entries.length, true);
  ev.setUint16(10, entries.length, true);
  ev.setUint32(12, cdSize, true);
  ev.setUint32(16, offset, true);

  const total = offset + cdSize + eocd.length;
  const out = new Uint8Array(total);
  let pos = 0;
  for (const p of [...localParts, ...centralParts, eocd]) {
    out.set(p, pos);
    pos += p.length;
  }
  return out;
}

export function buildDBeaverDbp(opts: {
  projectName: string;
  connectionId: string;
  dataSourcesJson: string;
  user: string;
  password?: string;
  iv?: Uint8Array;
}): Uint8Array {
  const enc = new TextEncoder();
  const safeProj = opts.projectName;
  const metaXml = [
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<archive version="1">',
    '  <source time="0" address="127.0.0.1" host="localhost"/>',
    "  <libraries/>",
    "  <projects>",
    `    <project name="${xmlEscape(safeProj)}" description="">`,
    '      <resource name=".project"/>',
    '      <resource name=".dbeaver">',
    '        <resource name="data-sources.json"/>',
    '        <resource name="credentials-config.json"/>',
    "      </resource>",
    "    </project>",
    "  </projects>",
    "</archive>",
    "",
  ].join("\n");

  const projectXml = [
    '<?xml version="1.0" encoding="UTF-8"?>',
    "<projectDescription>",
    `  <name>${xmlEscape(safeProj)}</name>`,
    "  <comment></comment>",
    "  <projects/>",
    "  <buildSpec/>",
    "  <natures>",
    "    <nature>org.jkiss.dbeaver.DBeaverNature</nature>",
    "  </natures>",
    "</projectDescription>",
    "",
  ].join("\n");

  const connCreds: Record<string, string> = { user: opts.user };
  if (opts.password !== undefined) connCreds.password = opts.password;
  const credsJson = JSON.stringify({ [opts.connectionId]: { "#connection": connCreds } });
  const encryptedCreds = encryptDBeaverCredentials(credsJson, opts.iv);

  const prefix = `projects/${safeProj}`;
  return buildZip([
    { name: "meta.xml", data: enc.encode(metaXml) },
    { name: `${prefix}/.project`, data: enc.encode(projectXml) },
    { name: `${prefix}/.dbeaver/`, data: new Uint8Array(0), isDir: true },
    { name: `${prefix}/.dbeaver/data-sources.json`, data: enc.encode(opts.dataSourcesJson) },
    { name: `${prefix}/.dbeaver/credentials-config.json`, data: encryptedCreds },
  ]);
}
