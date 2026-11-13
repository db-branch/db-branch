import { describe, expect, it } from "vitest";
import { planLabel } from "./modules";

describe("planLabel", () => {
  it("names the plans and what they include", () => {
    expect(planLabel("lite")).toBe("Lite · history, diff (beta)");
    expect(planLabel("team")).toContain("Team");
    expect(planLabel("team")).toContain("diff");
    expect(planLabel("business")).toContain("SSO");
    expect(planLabel("business")).not.toContain("masking");
  });
  it("reads a licence without a plan as business and keeps an unknown one", () => {
    expect(planLabel(undefined)).toContain("Business");
    expect(planLabel("mystery")).toBe("mystery");
  });
});
