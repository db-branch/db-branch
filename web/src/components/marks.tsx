// The console's visual vocabulary: every screen uses these marks the same way.
import { useEffect, useRef, useState } from "react";
import { bytes, engineLabel, statusShape, storageShare, ttlState } from "../lib/format";
import { t } from "../lib/i18n";

export function Status({ status, reason, tone }: { status: string; reason?: string | null; tone?: "soon" | "expired" }) {
  const shape = statusShape(status);
  const label = reason ? `${status}: ${reason}` : status;
  return (
    <svg className={`status status-${shape} status-${status}${tone ? ` status-${tone}` : ""}`} width="12" height="12" viewBox="0 0 12 12" role="img" aria-label={label}>
      <title>{label}</title>
      {shape === "dot" && <circle cx="6" cy="6" r="4" />}
      {shape === "ring" && <circle cx="6" cy="6" r="4" pathLength="100" strokeDasharray="70 30" />}
      {shape === "triangle" && <path d="M6 1.5 L10.5 10 H1.5 Z" />}
      {shape === "dashed" && <circle cx="6" cy="6" r="4" strokeDasharray="2 2" />}
      {shape === "pause" && (
        <>
          <rect x="2.5" y="2" width="2.5" height="8" />
          <rect x="7" y="2" width="2.5" height="8" />
        </>
      )}
    </svg>
  );
}

export function Engine({ runtime }: { runtime: string }) {
  const e = engineLabel(runtime);
  return (
    <span className={`engine engine-${e.engine}`} title={runtime}>
      <span className="engine-mark">{e.short}</span>
      <span className="mono">{e.version}</span>
    </span>
  );
}

export function TtlBar({ created, expires }: { created: string; expires: string | null }) {
  const st = ttlState(created, expires);
  if (st.kind === "none") return <span className="ttl ttl-none" title={t("No expiry")}>∞</span>;
  if (st.kind === "expired") return <span className="ttl ttl-expired" title={t("Expired {when}", { when: expires ?? "" })}>0</span>;
  return (
    <span className={`ttl ${st.warn ? "ttl-warn" : ""}`} title={t("Expires {when}", { when: new Date(expires!).toLocaleString() })}>
      <span className="bar">
        <span className="bar-fill" style={{ width: `${Math.max(2, st.fraction * 100)}%` }} />
      </span>
      <span className="mono ttl-left">{st.left}</span>
    </span>
  );
}

export function StorageBar({ storage, max }: { storage: { used: number; referenced: number } | null; max: number }) {
  const s = storageShare(storage, max);
  if (!storage) return <span className="muted">—</span>;
  return (
    <span className="storage" title={`${bytes(storage.used)} own · ${bytes(storage.referenced)} total (shared with base)`}>
      <span className="bar">
        <span className="bar-shared" style={{ width: `${s.total * 100}%` }} />
        <span className="bar-own" style={{ width: `${Math.max(1, s.own * 100)}%` }} />
      </span>
      <span className="mono">{bytes(storage.used)}</span>
    </span>
  );
}

export function Icon({ name }: { name: "plus" | "expand" | "copy" | "check" | "more" | "out" | "key" | "up" | "down" }) {
  const paths: Record<string, string> = {
    plus: "M8 3v10M3 8h10",
    expand: "M9.5 3.5h3v3M12.5 3.5 9 7M6.5 12.5h-3v-3M3.5 12.5 7 9",
    copy: "M5.5 5.5h7v7h-7zM3.5 10.5v-7h7",
    check: "M3.5 8.5l3 3 6-7",
    more: "M3.5 8h.01M8 8h.01M12.5 8h.01",
    out: "M9.5 3.5h3v9h-3M6.5 5.5 4 8l2.5 2.5M4 8h6",
    up: "M4 9.5l4-4 4 4",
    down: "M4 6.5l4 4 4-4",
    key: "M10 3.5a2.5 2.5 0 1 1 0 5 2.5 2.5 0 0 1 0-5zM8.2 7.8 3.5 12.5M5 11l1.5 1.5",
  };
  return (
    <svg className="icon" width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
      <path d={paths[name]} />
    </svg>
  );
}

// CopyButton copies what `load` returns. It always shows that it worked (the
// icon turns into a check); `text` makes it a labelled button, `showState`
// adds a "Copied" tag next to an icon button.
export function CopyButton({ load, label, text, showState, disabled }: { load: () => Promise<string>; label: string; text?: string; showState?: boolean; disabled?: boolean }) {
  const [state, setState] = useState<"idle" | "done" | "failed">("idle");
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const say = state === "done" ? t("Copied") : state === "failed" ? t("Copy failed") : "";
  return (
    <>
      <button
        type="button"
        className={text ? `copy-text ${state}` : "icon-button"}
        title={label}
        aria-label={label}
        disabled={disabled}
        onClick={async () => {
          try {
            await navigator.clipboard.writeText(await load());
            setState("done");
          } catch {
            setState("failed");
          }
          clearTimeout(timer.current);
          timer.current = setTimeout(() => setState("idle"), 1800);
        }}
      >
        <Icon name={state === "done" ? "check" : "copy"} />
        {text && <span aria-hidden="true">{say || text}</span>}
        {!text && showState && say && <span className={`copied-tag ${state}`} aria-hidden="true">{say}</span>}
      </button>
      <span className="sr-only" role="status">{say}</span>
    </>
  );
}
