// Live data: one EventSource for the whole console; every change event
// triggers a refetch of the views that care about that resource.
import { useCallback, useEffect, useRef, useState } from "react";

type Listener = (type: string) => void;
const listeners = new Set<Listener>();
let source: EventSource | null = null;

function connect() {
  if (source) return;
  source = new EventSource("/v1/events");
  const forward = (e: MessageEvent) => listeners.forEach((l) => l(e.type));
  for (const t of ["branch.updated", "branch.deleted", "base.updated", "base.deleted", "runtime.updated", "runtime.deleted", "operation.updated", "update.updated", "resync"]) {
    source.addEventListener(t, forward);
  }
  source.onerror = () => {
    // EventSource reconnects by itself and replays from Last-Event-ID.
  };
}

export function disconnect() {
  source?.close();
  source = null;
}

/** useLive loads data and reloads it whenever an event of one of the given resources arrives. */
export function useLive<T>(load: () => Promise<T>, resources: string[]): { data: T | undefined; error: string | null; reload: () => void } {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<string | null>(null);
  const loadRef = useRef(load);
  loadRef.current = load;
  const reload = useCallback(() => {
    loadRef.current().then(
      (d) => {
        setData(d);
        setError(null);
      },
      (e: Error) => setError(e.message),
    );
  }, []);
  const key = resources.join(",");
  useEffect(() => {
    connect();
    reload();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const l: Listener = (type) => {
      if (type === "resync" || key.split(",").some((r) => type.startsWith(r + "."))) {
        clearTimeout(timer);
        timer = setTimeout(reload, 100); // coalesce bursts
      }
    };
    listeners.add(l);
    return () => {
      listeners.delete(l);
      clearTimeout(timer);
    };
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
