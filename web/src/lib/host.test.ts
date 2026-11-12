import { describe, expect, it } from "vitest";
import { apiHost } from "./host";

describe("apiHost", () => {
  it("is the page address by default", () => {
    expect(apiHost({ location: { host: "node1:7080" } })).toBe("node1:7080");
  });

  it("is the host the page names when it names one", () => {
    expect(apiHost({ dbbHost: "node1.example.com:7080", location: { host: "demo.db-branch.com" } })).toBe("node1.example.com:7080");
  });
});
