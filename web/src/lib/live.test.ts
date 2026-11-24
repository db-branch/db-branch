import { afterEach, describe, expect, it, vi } from "vitest";
import { api, onUnauthorized } from "../api";
import { disconnect, subscribeEvents } from "./live";

class MockEventSource {
  static CLOSED = 2;
  static instances: MockEventSource[] = [];
  readyState = 1;
  closed = false;
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
});
