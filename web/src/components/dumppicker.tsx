import { ReactNode, useEffect, useRef, useState } from "react";
import { api, uploadDump } from "../api";
import { Field } from "./ui";
import { LocalizedError } from "../lib/errors";
import { bytes } from "../lib/format";
import { t } from "../lib/i18n";

// DumpPicker chooses where data comes from: a file on this computer
// (uploaded to the node, size checked first), or a path on the node.
export type DumpSource = { kind: "none" } | { kind: "file"; file: File | null } | { kind: "path"; path: string };

export function DumpPicker({ value, onChange, allowNone, node, base }: { value: DumpSource; onChange: (v: DumpSource) => void; allowNone: boolean; node?: string; base?: string }) {
  const [room, setRoom] = useState<{ max: number; reason: string } | null>(null);
  const input = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (value.kind !== "file") return;
    api.uploadRoom(node, base).then(
      (r) => setRoom({ max: r.max_bytes, reason: r.reason }),
      () => setRoom(null),
    );
  }, [value.kind, node, base]);
  const kinds: [DumpSource["kind"], string][] = [...(allowNone ? [["none", "Empty"] as [DumpSource["kind"], string]] : []), ["file", "Upload file"], ["path", "Path on node"]];
  const file = value.kind === "file" ? value.file : null;
  return (
    <>
      <Field label={t("Data")}>
        <div className="segmented" role="radiogroup">
          {kinds.map(([k, l]) => (
            <button
              type="button"
              key={k}
              role="radio"
              aria-checked={value.kind === k}
              className={value.kind === k ? "on" : ""}
              onClick={() => {
                if (k === value.kind) return k === "file" && input.current?.click();
                onChange(k === "none" ? { kind: "none" } : k === "path" ? { kind: "path", path: "" } : { kind: "file", file: null });
                if (k === "file") input.current?.click();
              }}
            >
              {l}
            </button>
          ))}
        </div>
      </Field>
      <input
        ref={input}
        type="file"
        accept=".sql,.gz,application/sql,application/gzip"
        hidden
        onChange={(e) => {
          const f = e.target.files?.[0];
          if (f) onChange({ kind: "file", file: f });
          e.target.value = "";
        }}
      />
      {file && (
        <div className="file-pick">
          <span className="mono">{file.name}</span>
          <span className="muted">{bytes(file.size)}</span>
          <button type="button" className="text-button" onClick={() => input.current?.click()}>
            {t("Change")}
          </button>
          {room && file.size > room.max && (
            <p className="reason">
              {t("Too large: this node accepts at most {max} now ({reason}).", {
                max: bytes(room.max),
                reason: room.reason === "pool" ? t("free space in the ZFS pool") : t("upload limit or free disk; see Settings"),
              })}
            </p>
          )}
          {!/\.sql(\.gz)?$/i.test(file.name) && <p className="reason">{t("Choose a .sql or .sql.gz file.")}</p>}
        </div>
      )}
      {value.kind === "file" && !file && <p className="muted small">{t("A .sql or .sql.gz dump (mysqldump / pg_dump plain format).")}</p>}
      {value.kind === "path" && (
        <Field label={t("Path")} hint={t("On the node. .sql or .sql.gz.")}>
          <input className="mono" value={value.path} onChange={(e) => onChange({ kind: "path", path: e.target.value })} placeholder="/srv/dumps/prod-masked.sql.gz" />
        </Field>
      )}
    </>
  );
}

export function dumpProblem(v: DumpSource, room: number | null): string | null {
  if (v.kind === "path" && !v.path.trim()) return "path";
  if (v.kind === "file" && (!v.file || !/\.sql(\.gz)?$/i.test(v.file.name) || (room !== null && v.file.size > room))) return "file";
  return null;
}

// useDumpSender uploads the chosen file (with progress and cancel) and
// returns the dump reference the API takes.
export function useDumpSender() {
  const [progress, setProgress] = useState<number | null>(null);
  const abort = useRef<AbortController | null>(null);
  const send = async (v: DumpSource, node?: string, base?: string): Promise<string | undefined> => {
    if (v.kind === "none") return undefined;
    if (v.kind === "path") return v.path.trim();
    if (!v.file) throw new LocalizedError(t("Choose a dump file."));
    const room = await api.uploadRoom(node, base);
    if (v.file.size > room.max_bytes) throw new LocalizedError(t("The file is {size}; the node accepts at most {max} now.", { size: bytes(v.file.size), max: bytes(room.max_bytes) }));
    abort.current = new AbortController();
    setProgress(0);
    try {
      const u = await uploadDump(v.file!, node, setProgress, abort.current.signal, base);
      return u.id;
    } finally {
      setProgress(null);
      abort.current = null;
    }
  };
  return { send, progress, cancel: () => abort.current?.abort() };
}

export function Progress({ value, onCancel }: { value: number; onCancel: () => void }): ReactNode {
  return (
    <div className="upload-progress">
      <div className="bar" role="progressbar" aria-valuenow={Math.round(value * 100)} aria-valuemin={0} aria-valuemax={100}>
        <span style={{ width: `${value * 100}%` }} />
      </div>
      <span className="mono">{Math.round(value * 100)}%</span>
      <button type="button" className="text-button" onClick={onCancel}>
        {t("Cancel")}
      </button>
    </div>
  );
}
