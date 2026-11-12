import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { DemoBackend } from "./backend";
import { fakeFetch, makeEventSource, makeXHR } from "./shim";


const ORIGIN = "https://demo.example";
let backend: DemoBackend;

beforeEach(() => {
  vi.useFakeTimers();
  backend = new DemoBackend();
  backend.signIn("admin");
});
afterEach(() => vi.useRealTimers());

describe("fakeFetch", () => {
  const real = vi.fn(async () => new Response("asset"));
  const f = () => fakeFetch(backend, real as unknown as typeof fetch, ORIGIN, () => 50);
  const settle = async <T>(p: Promise<T>) => {
    await vi.advanceTimersByTimeAsync(60);
    return p;
  };

  it("answers /v1 calls from the backend after a short delay", async () => {
    const p = f()("/v1/cluster");
    let done = false;
    void p.then(() => (done = true));
    await vi.advanceTimersByTimeAsync(10);
    expect(done).toBe(false);
    const res = await settle(p);
    expect(res.status).toBe(200);
    expect(((await res.json()) as { nodes: unknown[] }).nodes).toHaveLength(3);
    expect(real).not.toHaveBeenCalled();
  });
  it("sends JSON bodies and the method to the backend", async () => {
    const res = await settle(f()("/v1/branches", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ name: "via-fetch", base: "orders" }) }));
    expect(res.status).toBe(202);
    expect(backend.state.branches.some((b) => b.name === "via-fetch")).toBe(true);
  });
  it("returns an empty body for 204 and the error body for failures", async () => {
    const del = await settle(f()("/v1/tokens/tk-1", { method: "DELETE" }));
    expect(del.status).toBe(204);
    expect(await del.text()).toBe("");
    const bad = await settle(f()("/v1/branches/nope"));
    expect(bad.status).toBe(404);
    expect(((await bad.json()) as { error: { code: string } }).error.code).toBe("not_found");
  });
  it("accepts a Request object", async () => {
    const res = await settle(f()(new Request(ORIGIN + "/v1/runtimes")));
    expect(res.status).toBe(200);
  });
  it("lets the page's own files through but refuses other hosts", async () => {
    await f()("/assets/app.js");
    expect(real).toHaveBeenCalledTimes(1);
    await expect(f()("https://example.com/v1/cluster")).rejects.toThrow(/network access is disabled/);
    await expect(f()("https://static.cloudflareinsights.com/x")).rejects.toThrow(/disabled/);
  });
  it("lets the analytics beacon report to Cloudflare, and only that", async () => {
    await f()("https://cloudflareinsights.com/cdn-cgi/rum", { method: "POST", body: "{}" });
    expect(real).toHaveBeenCalledTimes(1);
  });
});

describe("EventSource stand-in", () => {
  it("delivers only the event types that were listened for, asynchronously", async () => {
    const ES = makeEventSource(backend);
    const es = new ES("/v1/events");
    const seen: [string, unknown][] = [];
    es.addEventListener("branch.updated", (e) => seen.push([e.type, JSON.parse(e.data)]));
    backend.handle({ method: "POST", path: "/v1/branches/report-fix/reset" });
    expect(seen).toEqual([]);
    await vi.advanceTimersByTimeAsync(1);
    expect(seen).toEqual([["branch.updated", { name: "report-fix" }]]);
    await vi.advanceTimersByTimeAsync(1400);
    expect(seen).toHaveLength(2); // and again when the reset is done
  });
  it("stops after close and after removeEventListener", async () => {
    const ES = makeEventSource(backend);
    const es = new ES("/v1/events");
    const a = vi.fn();
    const b = vi.fn();
    es.addEventListener("resync", a);
    es.addEventListener("resync", b);
    es.removeEventListener("resync", b);
    backend.signIn("admin");
    await vi.advanceTimersByTimeAsync(1);
    expect(a).toHaveBeenCalledTimes(1);
    expect(b).not.toHaveBeenCalled();
    es.close();
    expect(es.readyState).toBe(2);
    backend.signIn("admin");
    await vi.advanceTimersByTimeAsync(1);
    expect(a).toHaveBeenCalledTimes(1);
  });
});

class RealXHR {
  static sent: { method: string; url: string; headers: Record<string, string> }[] = [];
  status = 204;
  statusText = "No Content";
  responseText = "";
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onabort: (() => void) | null = null;
  private rec = { method: "", url: "", headers: {} as Record<string, string> };
  open(method: string, url: string) {
    this.rec.method = method;
    this.rec.url = url;
  }
  setRequestHeader(k: string, v: string) {
    this.rec.headers[k] = v;
  }
  send() {
    RealXHR.sent.push(this.rec);
    setTimeout(() => this.onload?.(), 5);
  }
  abort() {
    this.onabort?.();
  }
}

describe("XMLHttpRequest stand-in", () => {
  const upload = (size: number, name = "dump.sql.gz") => {
    const XHR = makeXHR(backend, ORIGIN, RealXHR as unknown as typeof XMLHttpRequest);
    const x = new XHR();
    const progress: number[] = [];
    x.open("POST", `/v1/uploads?name=${name}&node=node-1`);
    x.setRequestHeader();
    x.upload.onprogress = (e) => progress.push(e.loaded / e.total);
    const done = new Promise<void>((r) => (x.onload = r));
    x.send({ size } as Blob);
    return { x, progress, done };
  };

  it("reports progress up to 100% and ends with the backend's 201", async () => {
    const { x, progress, done } = upload(200 * 1024 * 1024);
    await vi.advanceTimersByTimeAsync(2000);
    expect(progress.length).toBeGreaterThan(3);
    expect(progress.length).toBeLessThan(20);
    expect(x.status).toBe(0);
    await vi.advanceTimersByTimeAsync(6000);
    await done;
    expect(progress.at(-1)).toBe(1);
    expect(x.status).toBe(201);
    expect(JSON.parse(x.responseText)).toMatchObject({ name: "dump.sql.gz", size: 200 * 1024 * 1024, node: "node-1" });
  });
  it("takes at least a moment, even for a tiny file", async () => {
    const { x } = upload(10);
    await vi.advanceTimersByTimeAsync(1100);
    expect(x.status).toBe(0);
    await vi.advanceTimersByTimeAsync(200);
    expect(x.status).toBe(201);
  });
  it("reports the backend's refusal of a file that is too big", async () => {
    const { x, done } = upload(30 * 1024 ** 3);
    await vi.advanceTimersByTimeAsync(9000);
    await done;
    expect(x.status).toBe(413);
  });
  it("can be cancelled", async () => {
    const XHR = makeXHR(backend, ORIGIN, RealXHR as unknown as typeof XMLHttpRequest);
    const x = new XHR();
    const aborted = vi.fn();
    x.onabort = aborted;
    x.onload = vi.fn();
    x.open("POST", "/v1/uploads?name=a.sql");
    x.send({ size: 100 * 1024 * 1024 } as Blob);
    await vi.advanceTimersByTimeAsync(500);
    x.abort();
    await vi.advanceTimersByTimeAsync(9000);
    expect(aborted).toHaveBeenCalledTimes(1);
    expect(x.onload).not.toHaveBeenCalled();
    x.abort(); // a second abort does nothing
    expect(aborted).toHaveBeenCalledTimes(1);
  });
  it("hands requests to anything but /v1 (the analytics beacon) to the real XMLHttpRequest", async () => {
    RealXHR.sent = [];
    const XHR = makeXHR(backend, ORIGIN, RealXHR as unknown as typeof XMLHttpRequest);
    const x = new XHR();
    const loaded = vi.fn();
    x.onload = loaded;
    x.open("POST", "https://cloudflareinsights.com/cdn-cgi/rum", true);
    x.setRequestHeader("Content-Type", "text/plain");
    x.send({ size: 2 } as Blob);
    await vi.advanceTimersByTimeAsync(10);
    expect(RealXHR.sent).toEqual([{ method: "POST", url: "https://cloudflareinsights.com/cdn-cgi/rum", headers: { "Content-Type": "text/plain" } }]);
    expect(loaded).toHaveBeenCalledTimes(1);
    expect(x.status).toBe(204);
    expect(backend.state.uploads).toEqual([]);
    x.abort();
  });
});
