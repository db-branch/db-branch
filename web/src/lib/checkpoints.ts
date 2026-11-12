// Branch checkpoints as the console shows them. Pure, so it can be tested
// without a browser; the server (domain.ParseCheckpointName) stays the authority.
import type { Checkpoint } from "../api";

export const MAX_CHECKPOINT_NAME = 40;

export type NameProblem = "chars" | "long" | null;

// checkpointNameProblem says what is wrong with a typed checkpoint name. The
// name is optional: nothing typed is no problem (the server uses the time).
export function checkpointNameProblem(name: string): NameProblem {
  if (name === "") return null;
  if (name.length > MAX_CHECKPOINT_NAME) return "long";
  return /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(name) ? null : "chars";
}

// newestFirst: the list the page shows, the latest on top.
export function newestFirst(list: Checkpoint[]): Checkpoint[] {
  return [...list].sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at) || (a.name < b.name ? 1 : -1));
}

// hasHistory: a restore has set a line of history aside, so the lines are worth showing.
export function hasHistory(list: Checkpoint[]): boolean {
  return list.some((c) => c.line > 0);
}

// notes: the badges of a checkpoint.
export function notes(c: Checkpoint): ("current" | "auto")[] {
  const out: ("current" | "auto")[] = [];
  if (c.current) out.push("current");
  if (c.auto) out.push("auto");
  return out;
}

export function savedLabel(name: string): string {
  return `Saved checkpoint ${name}`;
}
