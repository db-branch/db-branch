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
const INITIAL_RECONNECT_MS = 1000;
const MAX_RECONNECT_MS = 30000;
let source: EventSource | null = null;
let checkingAuth: Promise<void> | null = null;
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
let reconnectAttempts = 0;
let sessionSeq = 0;

function closeSource() {
  source?.close();
  source = null;
}

function scheduleReconnect() {
  if (reconnectTimer || listeners.size === 0) return;
  const base = Math.min(MAX_RECONNECT_MS, INITIAL_RECONNECT_MS * 2 ** reconnectAttempts);
  const delay = base + Math.floor(Math.random() * base * 0.2);
  reconnectAttempts++;
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    if (listeners.size > 0) connect();
  }, delay);
}

function handleDisconnect() {
  if (checkingAuth || reconnectTimer) return;
  const seq = sessionSeq;
  const hadSession = hasSession();
  checkingAuth = api
    .session()
    .then(
      () => {
        if (seq !== sessionSeq) return;
        if (source && typeof EventSource !== "undefined" && source.readyState === EventSource.CLOSED) {
          closeSource();
          scheduleReconnect();
        }
      },
      (err: unknown) => {
        if (seq !== sessionSeq) return;
        if (err instanceof ApiError && err.status === 401) {
          disconnect();
          if (!hadSession) notifyUnauthorized();
          return;
        }
        if (source && typeof EventSource !== "undefined" && source.readyState === EventSource.CLOSED) {
          closeSource();
          scheduleReconnect();
        }
      },
    )
    .finally(() => {
      if (seq === sessionSeq) checkingAuth = null;
    });
}

function connect() {
  if (source || reconnectTimer) return;
  source = new EventSource("/v1/events");
  source.onopen = () => {
    reconnectAttempts = 0;
  };
  const forward = (e: MessageEvent) => {
    reconnectAttempts = 0;
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
  sessionSeq++;
  checkingAuth = null;
  if (reconnectTimer) {
    clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }
  reconnectAttempts = 0;
  closeSource();
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
