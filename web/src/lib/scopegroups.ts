// Settings of a scope are grouped by the first segment of their keys
// (backup.*, branch.*, ...) when there are at least two of them.
import type { Setting } from "../api";

const labels: Record<string, string> = {
  backup: "Backups",
  branch: "Branches",
  notify: "Notifications",
  update: "Updates",
  console: "Console",
  zfs: "ZFS",
  ports: "Ports",
  upload: "Uploads",
  base: "Bases",
};

export type SettingGroup = { prefix: string; label: string; items: Setting[] };

const prefixOf = (key: string) => (key.includes(".") ? key.slice(0, key.indexOf(".")) : "");

export function groupSettings(settings: Setting[]): SettingGroup[] {
  const prefixes = Array.from(new Set(settings.map((s) => prefixOf(s.key)).filter(Boolean)));
  if (settings.length === 0) return [];
  if (prefixes.length < 2) return [{ prefix: "", label: "", items: settings }];
  // Keys without a dot come first, without a heading; the rest in order of appearance.
  const order = settings.some((s) => !prefixOf(s.key)) ? ["", ...prefixes] : prefixes;
  return order.map((prefix) => ({
    prefix,
    label: prefix ? (labels[prefix] ?? prefix.charAt(0).toUpperCase() + prefix.slice(1)) : "",
    items: settings.filter((s) => prefixOf(s.key) === prefix),
  }));
}
