// Stand-ins for the three browser APIs the console talks to the agent with:
// fetch (every call), EventSource (live updates) and XMLHttpRequest (dump
// upload progress). Each is built around a DemoBackend and touches no network.
import type { DemoBackend } from "./backend";
import { MIB } from "./state";

/** The one other host the demo may talk to: Cloudflare Web Analytics, which counts visits. */
export const ANALYTICS = "https://cloudflareinsights.com";

/** fakeFetch answers /v1 from the backend, passes other same-origin requests (the page's own files) and the analytics beacon on, and refuses everything else. */
export function fakeFetch(backend: DemoBackend, real: typeof fetch, origin: string, latency: () => number = () => 40 + Math.random() * 80): typeof fetch {
  return async (input, init) => {
    const req = input instanceof Request ? input : null;
    const url = new URL(req ? req.url : String(input), origin);
    if (url.origin === ANALYTICS) return real(input, init);
    if (url.origin !== origin) throw new TypeError("demo: network access is disabled");
    if (!url.pathname.startsWith("/v1/")) return real(input, init);
    let body: unknown;
    const raw = init?.body ?? (req ? await req.clone().text() : undefined);
    if (typeof raw === "string" && raw) {
      try {
        body = JSON.parse(raw);
      } catch {
        /* not JSON: the handlers see no body */
      }
    }
    await new Promise((r) => setTimeout(r, latency()));
    const r = backend.handle({ method: init?.method ?? req?.method ?? "GET", path: url.pathname + url.search, body });
    const empty = r.status === 204 || r.body === undefined;
    return new Response(empty ? null : JSON.stringify(r.body), { status: r.status, headers: { "Content-Type": "application/json" } });
  };
}

type Handler = (e: MessageEvent) => void;

/** makeEventSource returns a class like EventSource that receives the backend's events. */
export function makeEventSource(backend: DemoBackend) {
  return class FakeEventSource {
    static readonly CONNECTING = 0;
    static readonly OPEN = 1;
    static readonly CLOSED = 2;
    readyState = 1;
    onerror: ((e: Event) => void) | null = null;
    onopen: ((e: Event) => void) | null = null;
    onmessage: Handler | null = null;
    private handlers = new Map<string, Set<Handler>>();
    private stop: () => void;
    constructor(readonly url: string) {
      this.stop = backend.on((type, data) => {
        const hs = this.handlers.get(type);
        if (!hs) return;
        const e = new MessageEvent(type, { data: JSON.stringify(data) });
        setTimeout(() => this.readyState === 1 && hs.forEach((h) => h(e)), 0);
      });
    }
    addEventListener(type: string, h: Handler) {
      if (!this.handlers.has(type)) this.handlers.set(type, new Set());
      this.handlers.get(type)!.add(h);
    }
    removeEventListener(type: string, h: Handler) {
      this.handlers.get(type)?.delete(h);
    }
    close() {
      this.readyState = 2;
      this.stop();
    }
  };
}

type Progress = { lengthComputable: boolean; loaded: number; total: number };

/** UPLOAD_RATE is how fast a pretend upload goes (bytes per second). */
export const UPLOAD_RATE = 40 * MIB;

/** makeXHR returns a class like XMLHttpRequest for the dump upload: it reports progress over a time that fits the file's size, reads only its size, and finishes with the backend's answer. A request to anything but /v1 goes to the real XMLHttpRequest. */
export function makeXHR(backend: DemoBackend, origin: string, Real: typeof XMLHttpRequest) {
  return class FakeXHR {
    upload: { onprogress: ((e: Progress) => void) | null } = { onprogress: null };
    onload: (() => void) | null = null;
    onerror: (() => void) | null = null;
    onabort: (() => void) | null = null;
    status = 0;
    statusText = "";
    responseText = "";
    private method = "GET";
    private url = "";
    private real: XMLHttpRequest | null = null;
    private timer: ReturnType<typeof setInterval> | undefined;
    open(method: string, url: string, ...rest: [boolean?, string?, string?]) {
      this.method = method;
      this.url = url;
      if (new URL(url, origin).pathname.startsWith("/v1/")) return;
      this.real = new Real();
      this.real.open(method, url, ...(rest as [boolean]));
    }
    setRequestHeader(name?: string, value?: string) {
      if (name !== undefined && value !== undefined) this.real?.setRequestHeader(name, value);
    }
    send(body: Blob) {
      if (this.real) return this.sendReal(this.real, body);
      const total = body.size; // the content is never read
      const ms = Math.min(8000, Math.max(1200, (total / UPLOAD_RATE) * 1000));
      const steps = 20;
      let n = 0;
      this.timer = setInterval(() => {
        n++;
        this.upload.onprogress?.({ lengthComputable: true, loaded: (total * n) / steps, total });
        if (n < steps) return;
        clearInterval(this.timer);
        const r = backend.handle({ method: this.method, path: this.url, body: { size: total } });
        this.status = r.status;
        this.responseText = JSON.stringify(r.body ?? {});
        this.onload?.();
      }, ms / steps);
    }
    private sendReal(real: XMLHttpRequest, body: Blob) {
      const done = (call: (() => void) | null) => () => {
        this.status = real.status;
        this.statusText = real.statusText;
        this.responseText = real.responseText;
        call?.();
      };
      real.onload = done(this.onload);
      real.onerror = done(this.onerror);
      real.onabort = done(this.onabort);
      real.send(body);
    }
    abort() {
      if (this.real) return this.real.abort();
      if (this.timer === undefined) return;
      clearInterval(this.timer);
      this.timer = undefined;
      this.onabort?.();
    }
  };
}
