import { describe, expect, it } from "vitest";
import { planLabel } from "./modules";

describe("TestModules_プラン機能表記がlicense仕様と一致する", () => {
  it("Lite/Team/Business に diff (beta) を含めず、Business に history を含む", () => {
    expect(planLabel("lite")).toBe("Lite · history");
    expect(planLabel("team")).toBe("Team · roles, teams, history");
    expect(planLabel("business")).toBe("Business · roles, teams, SSO, audit, history");
    expect(planLabel("lite")).not.toContain("diff");
    expect(planLabel("team")).not.toContain("diff");
    expect(planLabel("business")).not.toContain("masking");
  });
  it("reads a licence without a plan as business and keeps an unknown one", () => {
    expect(planLabel(undefined)).toContain("Business");
    expect(planLabel("mystery")).toBe("mystery");
  });
});
