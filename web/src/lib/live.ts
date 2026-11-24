// Live data: one EventSource for the whole console; every change event
// triggers a refetch of the views that care about that resource.
import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError, hasSession, notifyUnauthorized } from "../api";
import { forgetCapabilities } from "./caps";

export const LIVE_EVENTS = [
  "branch.updated",
  "branch.deleted",
  "base.updated",
  "base.deleted",
  "runtime.updated",
  "runtime.deleted",
  "operation.updated",
  "update.updated",
  "setting.updated",
  "module.updated",
  "resync",
] as const;

type Listener = (type: string) => void;
const listeners = new Set<Listener>();
let source: EventSource | null = null;
let checkingAuth: Promise<void> | null = null;

function handleDisconnect() {
  if (checkingAuth) return;
  const hadSession = hasSession();
  checkingAuth = api
    .session()
    .then(
      () => {
        if (source && typeof EventSource !== "undefined" && source.readyState === EventSource.CLOSED) {
          disconnect();
          if (listeners.size > 0) connect();
        }
      },
      (err: unknown) => {
        if (err instanceof ApiError && err.status === 401) {
          disconnect();
          if (!hadSession) notifyUnauthorized();
        }
      },
    )
    .finally(() => {
      checkingAuth = null;
    });
}

function connect() {
  if (source) return;
  source = new EventSource("/v1/events");
  const forward = (e: MessageEvent) => {
    if (e.type === "module.updated" || e.type === "resync") {
      forgetCapabilities();
    }
    listeners.forEach((l) => l(e.type));
  };
  for (const t of LIVE_EVENTS) {
    source.addEventListener(t, forward);
  }
  source.onerror = () => {
    // EventSource does not expose HTTP status; probe /v1/auth/session so 401
    // stops reconnecting and sends the console back to login.
    handleDisconnect();
  };
}

export function disconnect() {
  source?.close();
  source = null;
}

/** subscribeEvents opens the shared EventSource and invokes onReload (coalesced) when any matching resource or resync arrives. */
export function subscribeEvents(resources: string[], onReload: () => void): () => void {
  connect();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const l: Listener = (type) => {
    if (type === "resync" || resources.some((r) => type.startsWith(r + "."))) {
      clearTimeout(timer);
      timer = setTimeout(onReload, 100); // coalesce bursts
    }
  };
  listeners.add(l);
  return () => {
    listeners.delete(l);
    clearTimeout(timer);
  };
}

/** useLive loads data and reloads it whenever an event of one of the given resources arrives. */
export function useLive<T>(load: () => Promise<T>, resources: string[]): { data: T | undefined; error: unknown; reload: () => void } {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<unknown>(null);
  const loadRef = useRef(load);
  loadRef.current = load;
  const reload = useCallback(() => {
    loadRef.current().then(
      (d) => {
        setData(d);
        setError(null);
      },
      (e: unknown) => setError(e),
    );
  }, []);
  const key = resources.join(",");
  useEffect(() => {
    reload();
    return subscribeEvents(key.split(","), reload);
  }, [key, reload]);
  return { data, error, reload };
}

/** useNow re-renders every interval so relative times and TTL bars move. */
export function useNow(intervalMs = 30000): number {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(t);
  }, [intervalMs]);
  return now;
}
