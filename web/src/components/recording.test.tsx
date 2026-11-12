import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Choice } from "../lib/bases";
import { NewBranch } from "./newbranch";
import { RecordingBadge, RecordingValue } from "./recording";

const html = (node: React.ReactElement) => renderToStaticMarkup(node);

describe("Recording is shown, never edited, on a Branch that exists", () => {
  it("says on, since creation, for a recorded Branch and off for the others", () => {
    expect(html(<RecordingValue record />)).toContain("on <span");
    expect(html(<RecordingValue record />)).toContain("since creation");
    expect(html(<RecordingValue />)).toBe("off");
    expect(html(<RecordingValue record={false} />)).toBe("off");
  });
  it("marks only a recorded Branch, and the tooltip says it is from creation", () => {
    expect(html(<RecordingBadge record />)).toContain("recording");
    expect(html(<RecordingBadge record />)).toContain("since the Branch was created");
    expect(html(<RecordingBadge />)).toBe("");
  });
  it("has no checkbox, button or input in either", () => {
    const h = html(<><RecordingBadge record /><RecordingValue record /></>);
    expect(h).not.toMatch(/<input|<button|<select/);
  });
});

const choice: Choice = { name: "orders", runtime: "mysql@8.4.13", description: "", snapshot: "s1", snapshotAt: null, public: false, masked: false };
const form = (over: Partial<Parameters<typeof NewBranch>[0]> = {}) => html(<NewBranch choices={[choice]} signedIn canRecord onStarted={() => {}} {...over} />);

describe("the Record checkbox exists only in the create form", () => {
  it("offers it, with the note that it cannot be changed later", () => {
    const h = form();
    expect(h).toContain('type="checkbox"');
    expect(h).toContain("cannot be turned on or off after the Branch is created");
  });
  it("does not offer it without the module, without a login or for PostgreSQL", () => {
    expect(form({ canRecord: false })).not.toContain('type="checkbox"');
    expect(form({ signedIn: false })).not.toContain('type="checkbox"');
    expect(form({ choices: [{ ...choice, runtime: "postgres@17.0" }] })).not.toContain('type="checkbox"');
  });
});
