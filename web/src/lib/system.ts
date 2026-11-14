// How a node's system is shown: each fact once. Pure.
import type { NodeSystem } from "../api";

export type Label = { text: string; title: string };

// osLabel: the OS name; the distribution key ("ubuntu-24.04") says the same, so it is the tooltip.
export function osLabel(system?: Pick<NodeSystem, "os" | "distro">): Label {
  const distro = system?.distro && system.distro !== "unknown" ? system.distro : "";
  const text = system?.os || distro || "unknown";
  return { text, title: distro || text };
}

const upstream = (v: string) => v.split("-")[0];

// zfsLabel: "2.2.2-0ubuntu9.5 (kmod 2.2.2-0ubuntu9.4)" is one release; both are shown only when they differ.
export function zfsLabel(zfs?: string): Label {
  if (!zfs) return { text: "unknown", title: "unknown" };
  const m = zfs.match(/^(\S+) \(kmod (\S+)\)$/);
  if (!m) return { text: upstream(zfs), title: zfs };
  const [, tools, kmod] = m;
  return { text: upstream(tools) === upstream(kmod) ? upstream(tools) : `${upstream(tools)} (kmod ${upstream(kmod)})`, title: zfs };
}
