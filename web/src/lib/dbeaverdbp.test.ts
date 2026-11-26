import { describe, expect, it, vi } from "vitest";
import { buildDBeaverDbp, buildZip, encryptDBeaverCredentials, xmlEscape } from "./dbeaverdbp";

const DBEAVER_LOCAL_KEY = new Uint8Array([0xba, 0xbb, 0x4a, 0x9f, 0x77, 0x4a, 0xb8, 0x53, 0xc9, 0x6c, 0x2d, 0x65, 0x3d, 0xfe, 0x54, 0x4a]);
const td = new TextDecoder();
const te = new TextEncoder();

async function decryptDBeaver(encrypted: Uint8Array): Promise<string> {
  const key = await crypto.subtle.importKey("raw", DBEAVER_LOCAL_KEY, { name: "AES-CBC" }, false, ["decrypt"]);
  const iv = Uint8Array.from(encrypted.subarray(0, 16));
  const body = Uint8Array.from(encrypted.subarray(16));
  const plain = await crypto.subtle.decrypt({ name: "AES-CBC", iv }, key, body);
  return td.decode(plain);
}

function parseZipEntries(buf: Uint8Array): Map<string, { data: Uint8Array; isDir: boolean }> {
  const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const out = new Map<string, { data: Uint8Array; isDir: boolean }>();
  let eocd = -1;
  for (let i = buf.length - 22; i >= 0; i--) {
    if (view.getUint32(i, true) === 0x06054b50) {
      eocd = i;
      break;
    }
  }
  expect(eocd).toBeGreaterThanOrEqual(0);
  const cdOffset = view.getUint32(eocd + 16, true);
  const total = view.getUint16(eocd + 10, true);
  let p = cdOffset;
  for (let i = 0; i < total; i++) {
    expect(view.getUint32(p, true)).toBe(0x02014b50);
    const compSize = view.getUint32(p + 20, true);
    const nameLen = view.getUint16(p + 28, true);
    const extraLen = view.getUint16(p + 30, true);
    const commentLen = view.getUint16(p + 32, true);
    const localOffset = view.getUint32(p + 42, true);
    const name = td.decode(buf.subarray(p + 46, p + 46 + nameLen));
    const lNameLen = view.getUint16(localOffset + 26, true);
    const lExtraLen = view.getUint16(localOffset + 28, true);
    const dataStart = localOffset + 30 + lNameLen + lExtraLen;
    const data = buf.slice(dataStart, dataStart + compSize);
    out.set(name, { data, isDir: name.endsWith("/") });
    p += 46 + nameLen + extraLen + commentLen;
  }
  return out;
}

describe("encryptDBeaverCredentials", () => {
  it("encrypts JSON using DBeaver AES-128-CBC local key with 16-byte IV prefix", async () => {
    const plain = JSON.stringify({ "conn-1": { "#connection": { user: "alice", password: "p@ssword1234567890" } } });
    const iv = Uint8Array.from({ length: 16 }, (_, i) => i + 1);
    const encrypted = encryptDBeaverCredentials(plain, iv);

    expect(encrypted.subarray(0, 16)).toEqual(iv);
    expect(await decryptDBeaver(encrypted)).toBe(plain);
  });

  it("throws instead of falling back to an all-zero IV when crypto.getRandomValues is unavailable", () => {
    vi.stubGlobal("crypto", undefined);
    try {
      expect(() => encryptDBeaverCredentials("{}")).toThrow(/Random number generation is unavailable|乱数生成/i);
    } finally {
      vi.unstubAllGlobals();
    }
  });
});

describe("xmlEscape", () => {
  it("escapes XML special characters and strips/replaces forbidden XML 1.0 control characters", () => {
    const out = xmlEscape(`a&b<c>"d"'e'\x00\x07\x0b\x0c\x1f\ufffe\uffff\t\n\r`);
    expect(out).not.toMatch(/[\x00-\x08\x0b\x0c\x0e-\x1f\ufffe\uffff]/);
    expect(out).toContain("a&amp;b&lt;c&gt;&quot;d&quot;&apos;e&apos;");
  });
});

describe("buildZip and buildDBeaverDbp", () => {
  it("builds a valid ZIP archive with meta.xml, .project, data-sources.json, and encrypted credentials-config.json", async () => {
    const dbp = buildDBeaverDbp({
      projectName: "db-branch-aaa1",
      connectionId: "db-branch-aaa1",
      dataSourcesJson: '{"connections":{}}',
      user: "aaa1",
      password: "secret",
      iv: Uint8Array.from({ length: 16 }, (_, i) => 0x10 + i),
    });

    const entries = parseZipEntries(dbp);
    expect(td.decode(entries.get("meta.xml")?.data)).toContain('<project name="db-branch-aaa1"');
    expect(td.decode(entries.get("meta.xml")?.data)).toContain('<resource name=".dbeaver">');
    expect(td.decode(entries.get("projects/db-branch-aaa1/.project")?.data)).toContain("org.jkiss.dbeaver.DBeaverNature");
    expect(entries.get("projects/db-branch-aaa1/.dbeaver/")?.isDir).toBe(true);
    expect(td.decode(entries.get("projects/db-branch-aaa1/.dbeaver/data-sources.json")?.data)).toBe('{"connections":{}}');

    const credBuf = entries.get("projects/db-branch-aaa1/.dbeaver/credentials-config.json")!.data;
    const credJson = JSON.parse(await decryptDBeaver(credBuf));
    expect(credJson).toEqual({
      "db-branch-aaa1": {
        "#connection": {
          user: "aaa1",
          password: "secret",
        },
      },
    });
  });

  it("omits password from credentials-config.json when password is undefined", async () => {
    const zip = buildZip([{ name: "a.txt", data: te.encode("hi") }]);
    expect(td.decode(parseZipEntries(zip).get("a.txt")?.data)).toBe("hi");

    const dbp = buildDBeaverDbp({
      projectName: "p",
      connectionId: "c1",
      dataSourcesJson: "{}",
      user: "u1",
      iv: new Uint8Array(16),
    });
    const entries = parseZipEntries(dbp);
    const credBuf = entries.get("projects/p/.dbeaver/credentials-config.json")!.data;
    const credJson = JSON.parse(await decryptDBeaver(credBuf));
    expect(credJson).toEqual({ c1: { "#connection": { user: "u1" } } });
  });
});
