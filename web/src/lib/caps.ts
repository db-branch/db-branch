// The server's capabilities, fetched once and shared by every component that
// asks (a page with many branch cards would otherwise ask once per card).
import { useEffect, useState } from "react";
import { api, Capabilities } from "../api";

let cached: { at: number; value: Promise<Capabilities> } | null = null;
const FRESH_MS = 30_000;

export function capabilities(): Promise<Capabilities> {
  if (!cached || Date.now() - cached.at > FRESH_MS) {
    const value = api.capabilities();
    cached = { at: Date.now(), value };
    value.catch(() => (cached = null));
  }
  return cached.value;
}

/** forgetCapabilities drops the shared answer (tests; a module change). */
export function forgetCapabilities() {
  cached = null;
}

/** useCapabilities returns the capabilities, or undefined until known (or when they cannot be had). Asks again when key changes (a login). */
export function useCapabilities(key?: unknown, enabled = true): Capabilities | undefined {
  const [caps, setCaps] = useState<Capabilities>();
  useEffect(() => {
    if (!enabled) {
      setCaps(undefined);
      return;
    }
    let live = true;
    setCaps(undefined);
    capabilities().then(
      (c) => live && setCaps(c),
      () => undefined,
    );
    return () => {
      live = false;
    };
  }, [key, enabled]);
  return caps;
}
