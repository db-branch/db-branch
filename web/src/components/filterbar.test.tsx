import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { FilterBar } from "./filterbar";

const noop = () => {};
const view = (props: Record<string, unknown> = {}) =>
  renderToStaticMarkup(<FilterBar values={{ q: "", status: "" }} onChange={noop} facets={[{ key: "status", label: "Status", options: ["failed", "ready"] }]} shown={3} total={10} {...props} />);

describe("FilterBar", () => {
  it("has a search box and one select per facet, with the options", () => {
    const h = view();
    expect(h).toContain('type="search"');
    expect(h).toContain('role="search"');
    expect(h).toContain("All status");
    expect(h).toContain(">failed<");
    expect(h).toContain(">ready<");
  });
  it("shows how many are shown of the total only while a filter is set, and a Clear button", () => {
    expect(view()).not.toContain("3 / 10");
    expect(view()).not.toContain("Clear");
    const h = view({ values: { q: "x", status: "" } });
    expect(h).toContain("3 / 10");
    expect(h).toContain("Clear");
  });
  it("marks the chosen option and leaves out a facet with no options", () => {
    expect(view({ values: { q: "", status: "ready" } })).toMatch(/<option value="ready" selected/);
    expect(view({ facets: [{ key: "node", label: "Node", options: [] }] })).not.toContain("All node");
  });
});
