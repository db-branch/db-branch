import { afterEach, describe, expect, it, vi } from "vitest";
import { api, onUnauthorized } from "../api";
import { disconnect, subscribeEvents } from "./live";

class MockEventSource {
  static CLOSED = 2;
  static instances: MockEventSource[] = [];
  readyState = 1;
  closed = false;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  private handlers = new Map<string, ((e: MessageEvent) => void)[]>();

  constructor(readonly url: string) {
    MockEventSource.instances.push(this);
  }
  addEventListener(type: string, fn: (e: MessageEvent) => void) {
    const list = this.handlers.get(type) ?? [];
    list.push(fn);
    this.handlers.set(type, list);
  }
  emit(type: string, data = "{}") {
    for (const fn of this.handlers.get(type) ?? []) {
      fn({ type, data } as MessageEvent);
    }
  }
  close() {
    this.closed = true;
    this.readyState = MockEventSource.CLOSED;
  }
}

describe("TestLive_settingUpdatedとmoduleUpdatedを受信して再読込し401切断時に再ログイン誘導する", () => {
  const origES = globalThis.EventSource;
  const origFetch = globalThis.fetch;

  afterEach(() => {
    disconnect();
    MockEventSource.instances = [];
    globalThis.EventSource = origES;
    globalThis.fetch = origFetch;
    vi.useRealTimers();
  });

  it("setting.updated と module.updated を購読し該当リソースを再読込する", () => {
    vi.useFakeTimers();
    globalThis.EventSource = MockEventSource as unknown as typeof EventSource;

    let settingReloads = 0;
    let moduleReloads = 0;
    const unsub1 = subscribeEvents(["setting"], () => {
      settingReloads++;
    });
    const unsub2 = subscribeEvents(["module"], () => {
      moduleReloads++;
    });

    const es = MockEventSource.instances[0];
    expect(es).toBeDefined();

    es.emit("setting.updated", JSON.stringify({ scope: "core" }));
    vi.advanceTimersByTime(120);
    expect(settingReloads).toBe(1);
    expect(moduleReloads).toBe(0);

    es.emit("module.updated", JSON.stringify({ name: "audit" }));
    vi.advanceTimersByTime(120);
    expect(settingReloads).toBe(1);
    expect(moduleReloads).toBe(1);

    unsub1();
    unsub2();
  });

  it("SSE 切断時に 401 なら EventSource を閉じ再ログインを促す", async () => {
    globalThis.EventSource = MockEventSource as unknown as typeof EventSource;
    globalThis.fetch = (async () =>
      new Response(JSON.stringify({ error: { code: "unauthenticated", message: "session expired" } }), { status: 401 })) as typeof fetch;

    let unauthorized = 0;
    const off = onUnauthorized(() => {
      unauthorized++;
    });
    const unsub = subscribeEvents(["branch"], () => {});
    const es = MockEventSource.instances[0];

    es.onerror?.(new Event("error"));
    await vi.waitFor(() => {
      expect(unauthorized).toBe(1);
    });
    expect(es.closed).toBe(true);

    off();
    unsub();
  });

  it("認証済みセッション中の API 401 でも再ログインを促す", async () => {
    let calls = 0;
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      calls++;
      if (String(input) === "/v1/auth/session") {
        return new Response(JSON.stringify({ user: { id: "admin", name: "admin", roles: ["admin"] }, csrf_token: "tok" }), { status: 200 });
      }
      return new Response(JSON.stringify({ error: { code: "unauthenticated", message: "expired" } }), { status: 401 });
    }) as typeof fetch;

    await api.session();
    let unauthorized = 0;
    const off = onUnauthorized(() => {
      unauthorized++;
    });
    await expect(api.branches()).rejects.toThrow("expired");
    expect(calls).toBe(2);
    expect(unauthorized).toBe(1);
    off();
  });

  it("SSE が 502 等で CLOSED になっても即再接続せず指数バックオフし復旧後・切断時にリセットする", async () => {
    vi.useFakeTimers();
    globalThis.EventSource = MockEventSource as unknown as typeof EventSource;
    let sessionStatus = 200;
    globalThis.fetch = (async () => {
      if (sessionStatus !== 200) {
        return new Response(JSON.stringify({ error: { code: "unavailable", message: "bad gateway" } }), { status: sessionStatus });
      }
      return new Response(JSON.stringify({ user: { id: "admin", name: "admin", roles: ["admin"] }, csrf_token: "tok" }), { status: 200 });
    }) as typeof fetch;

    const unsub = subscribeEvents(["branch"], () => {});
    expect(MockEventSource.instances).toHaveLength(1);

    // 1st CLOSED error: session probe returns 200 -> must NOT reconnect immediately.
    const es1 = MockEventSource.instances[0];
    es1.readyState = MockEventSource.CLOSED;
    es1.onerror?.(new Event("error"));
    await vi.advanceTimersByTimeAsync(500);
    expect(MockEventSource.instances).toHaveLength(1);

    // After ~1s initial backoff, 2nd EventSource connects.
    await vi.advanceTimersByTimeAsync(800);
    expect(MockEventSource.instances).toHaveLength(2);

    // 2nd CLOSED error while /v1/auth/session returns 502 -> still backs off (~2s), doesn't die or tight-loop.
    sessionStatus = 502;
    const es2 = MockEventSource.instances[1];
    es2.readyState = MockEventSource.CLOSED;
    es2.onerror?.(new Event("error"));
    await vi.advanceTimersByTimeAsync(1500);
    expect(MockEventSource.instances).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1200);
    expect(MockEventSource.instances).toHaveLength(3);

    // Opening connection resets backoff back to initial ~1s.
    sessionStatus = 200;
    const es3 = MockEventSource.instances[2];
    es3.onopen?.(new Event("open"));
    es3.readyState = MockEventSource.CLOSED;
    es3.onerror?.(new Event("error"));
    await vi.advanceTimersByTimeAsync(500);
    expect(MockEventSource.instances).toHaveLength(3);
    await vi.advanceTimersByTimeAsync(800);
    expect(MockEventSource.instances).toHaveLength(4);

    // disconnect() cancels pending reconnect timer.
    const es4 = MockEventSource.instances[3];
    es4.readyState = MockEventSource.CLOSED;
    es4.onerror?.(new Event("error"));
    await vi.advanceTimersByTimeAsync(100);
    disconnect();
    await vi.advanceTimersByTimeAsync(5000);
    expect(MockEventSource.instances).toHaveLength(4);

    // disconnect() while /v1/auth/session probe is in-flight must not block reconnection after re-subscribe.
    let resolveSlow!: (r: Response) => void;
    globalThis.fetch = (() =>
      new Promise<Response>((res) => {
        resolveSlow = res;
      })) as typeof fetch;
    const unsub2 = subscribeEvents(["branch"], () => {});
    const es5 = MockEventSource.instances[4];
    es5.readyState = MockEventSource.CLOSED;
    es5.onerror?.(new Event("error"));
    disconnect();
    globalThis.fetch = (async () =>
      new Response(JSON.stringify({ user: { id: "admin", name: "admin", roles: ["admin"] }, csrf_token: "tok" }), { status: 200 })) as typeof fetch;
    const unsub3 = subscribeEvents(["branch"], () => {});
    const es6 = MockEventSource.instances[5];
    es6.readyState = MockEventSource.CLOSED;
    es6.onerror?.(new Event("error"));
    resolveSlow(new Response("{}", { status: 200 }));
    await vi.advanceTimersByTimeAsync(1300);
    expect(MockEventSource.instances).toHaveLength(7);

    unsub();
    unsub2();
    unsub3();
  });
});
