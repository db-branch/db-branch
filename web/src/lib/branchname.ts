// Branch names made on the user page start with a prefix the server gives
// (the owner, see branch_prefix in the session). These mirror the server's
// rule (domain.PrefixedName); the server stays the authority.
export const MAX_NAME = 32;

// fullName is the branch name that results from typing `typed`.
export function fullName(prefix: string | undefined, typed: string): string {
  if (!typed || !prefix || typed.startsWith(prefix)) return typed;
  return prefix + typed;
}

// maxTyped is how many characters the typed text may have (a typed text that
// already starts with the prefix carries the prefix on top).
export function maxTyped(prefix: string | undefined, typed = ""): number {
  const p = prefix ?? "";
  return p && typed.startsWith(p) ? MAX_NAME : MAX_NAME - p.length;
}

// ownerPrefix mirrors domain.OwnerPrefix: the part of an owner (user name or
// email) that prefixes the branches that person makes on the user page.
export function ownerPrefix(owner: string): string {
  const local = owner.split("@")[0].toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  return local === "" || /^[0-9]/.test(local) ? "u" + local : local;
}

// splitOwned splits a branch name into the owner prefix (with its hyphen) and
// the rest, so the console can mute the prefix. The prefix is "" when the name
// does not start with it.
export function splitOwned(name: string, owner: string | undefined): [string, string] {
  if (!owner) return ["", name];
  const prefix = ownerPrefix(owner) + "-";
  return name.startsWith(prefix) && name.length > prefix.length ? [prefix, name.slice(prefix.length)] : ["", name];
}

const ALPHABET = "abcdefghijklmnopqrstuvwxyz0123456789";

// suggestName proposes a typed name for a branch made from `base`: the base
// name and three random characters. It fits MAX_NAME together with the prefix
// and always follows the name rule (a letter first, single hyphens).
export function suggestName(base: string, prefix: string | undefined, rand: () => number = Math.random): string {
  const room = MAX_NAME - (prefix ?? "").length;
  const suffix = Array.from({ length: 3 }, () => ALPHABET[Math.floor(rand() * ALPHABET.length)]).join("");
  if (room < 5) return ("b" + suffix).slice(0, Math.max(1, room));
  let stem = base.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  if (stem === "") stem = "branch";
  else if (/^[0-9]/.test(stem)) stem = "b" + stem;
  stem = stem.slice(0, room - 4).replace(/-+$/, "");
  return `${stem}-${suffix}`;
}

export type NameProblem = "start" | "chars" | "long" | null;

// nameProblem says what is wrong with the typed name, mirroring the server
// (domain.PrefixedName and the name pattern). Nothing typed is no problem yet.
export function nameProblem(prefix: string | undefined, typed: string): NameProblem {
  if (typed === "") return null;
  const p = prefix ?? "";
  const rest = p && typed.startsWith(p) ? typed.slice(p.length) : typed;
  if (rest.length > MAX_NAME - p.length) return "long";
  if (!/^[a-z]/.test(rest)) return "start";
  return /^[a-z](?:[a-z0-9]|-[a-z0-9])*$/.test(rest) ? null : "chars";
}
