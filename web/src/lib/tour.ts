// The first-visit tour of the user page: who sees it, whether they have seen
// it, which steps apply, and where the popover goes. No DOM access here, so
// it is testable; components/tour.tsx draws it.
import type { Session } from "../api";
import { t } from "./i18n";

export const tourKey = (userId: string) => `db-branch.tour.v1.${userId}`;

// showsTour: only a signed-in person who is not an administrator gets the tour.
export function showsTour(session: Session | null | undefined): session is Session {
  if (!session) return false;
  return !session.user.roles.some((r) => r === "admin" || r === "scope:full");
}

type Store = Pick<Storage, "getItem" | "setItem">;

// createSeenStore remembers, per user, that the tour was seen. When the browser
// refuses storage it falls back to memory: once per page load, never a crash.
export function createSeenStore(storage: () => Store) {
  const memory = new Set<string>();
  return {
    has(userId: string): boolean {
      if (memory.has(userId)) return true;
      try {
        return storage().getItem(tourKey(userId)) === "1";
      } catch {
        return false;
      }
    },
    mark(userId: string) {
      memory.add(userId);
      try {
        storage().setItem(tourKey(userId), "1");
      } catch {
        /* storage unavailable: remembered until the page reloads */
      }
    },
  };
}

export const seenTour = createSeenStore(() => localStorage);

export type TourStep = {
  id: string;
  // anchors: CSS selectors, tried in order; the first that exists is used.
  anchors: string[];
  title: string;
  body: string;
};

export function tourSteps(prefix?: string): TourStep[] {
  return [
    {
      id: "base",
      anchors: ["[data-tour=base]"],
      title: t("Choose a Base"),
      body: t("Pick the Base to copy. A Branch is cloned from a Snapshot of it, so you get the data as it was when the latest Snapshot was taken."),
    },
    {
      id: "name",
      anchors: ["[data-tour=name]"],
      title: t("Name your Branch"),
      body: prefix
        ? t("Every name starts with {prefix}, which is fixed for your account. The full name is also the database user name you connect with.", { prefix })
        : t("Pick a short name. The full name is also the database user name you connect with."),
    },
    {
      id: "ttl",
      anchors: ["[data-tour=ttl]"],
      title: t("Set the lifetime"),
      body: t("Choose how long the Branch lives. When its TTL runs out it is deleted; you can extend it before then."),
    },
    {
      id: "create",
      anchors: ["[data-tour=create]"],
      title: t("Create it"),
      body: t("Press \"Create branch\". Your Branch is ready in a few seconds."),
    },
    {
      id: "branches",
      anchors: ["[data-tour=branch]", "[data-tour=mine]"],
      title: t("Your branches"),
      body: t("Each Branch you make is listed here. Open one to see its connection details; the password stays hidden until you press \"Show password\". You can also extend, reset or delete a Branch."),
    },
  ];
}

export type ResolvedStep = TourStep & { anchor: string };

// resolveSteps keeps the steps whose target exists now, each with the selector to use.
export function resolveSteps(steps: TourStep[], exists: (selector: string) => boolean): ResolvedStep[] {
  const out: ResolvedStep[] = [];
  for (const s of steps) {
    const anchor = s.anchors.find(exists);
    if (anchor) out.push({ ...s, anchor });
  }
  return out;
}

type Box = { top: number; left: number; bottom: number; right: number };
type Size = { width: number; height: number };

const GAP = 12;
const MARGIN = 8;
const PHONE = 480;

// placePopover: below the target if it fits, else above, else pinned to the
// bottom edge; always inside the viewport. No target: centred.
export function placePopover(target: Box | null, size: Size, vp: Size): { top: number; left: number } {
  const clampLeft = (x: number) => Math.max(MARGIN, Math.min(x, vp.width - size.width - MARGIN));
  if (!target) return { top: Math.round((vp.height - size.height) / 2), left: Math.round((vp.width - size.width) / 2) };
  // A phone is too narrow to follow the target sideways: centre the card.
  const left = vp.width <= PHONE ? Math.round((vp.width - size.width) / 2) : clampLeft(target.left);
  if (target.bottom + GAP + size.height <= vp.height - MARGIN) return { top: target.bottom + GAP, left };
  if (target.top - GAP - size.height >= MARGIN) return { top: target.top - GAP - size.height, left };
  return { top: vp.height - size.height - MARGIN, left };
}
