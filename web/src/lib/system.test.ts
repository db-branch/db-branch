import { describe, expect, it } from "vitest";
import { osLabel, zfsLabel } from "./system";

describe("osLabel", () => {
  it("shows the OS name once: the distribution key says the same and stays in the tooltip", () => {
    expect(osLabel({ os: "Ubuntu 24.04.5 LTS", distro: "ubuntu-24.04" })).toEqual({ text: "Ubuntu 24.04.5 LTS", title: "ubuntu-24.04" });
  });
  it("falls back to unknown, and to the key when the name is missing", () => {
    expect(osLabel(undefined)).toEqual({ text: "unknown", title: "unknown" });
    expect(osLabel({ os: "", distro: "debian-13" })).toEqual({ text: "debian-13", title: "debian-13" });
  });
});

describe("zfsLabel", () => {
  it("shows one version when the module is the same release as the tools", () => {
    expect(zfsLabel("2.2.2-0ubuntu9.5 (kmod 2.2.2-0ubuntu9.4)")).toEqual({ text: "2.2.2", title: "2.2.2-0ubuntu9.5 (kmod 2.2.2-0ubuntu9.4)" });
    expect(zfsLabel("2.3.1")).toEqual({ text: "2.3.1", title: "2.3.1" });
  });
  it("shows both when the kernel module is another release", () => {
    expect(zfsLabel("2.2.2-0ubuntu9.5 (kmod 2.2.1-1)")).toEqual({ text: "2.2.2 (kmod 2.2.1)", title: "2.2.2-0ubuntu9.5 (kmod 2.2.1-1)" });
  });
  it("says unknown for nothing", () => {
    expect(zfsLabel(undefined)).toEqual({ text: "unknown", title: "unknown" });
    expect(zfsLabel("")).toEqual({ text: "unknown", title: "unknown" });
  });
});
