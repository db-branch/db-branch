import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { Modal } from "./modal";

const html = (over: Partial<Parameters<typeof Modal>[0]> = {}) =>
  renderToStaticMarkup(
    <Modal title="Checkpoints of x" onClose={() => {}} {...over}>
      <p>inside</p>
    </Modal>,
  );

describe("Modal", () => {
  it("is a modal dialog named by its title", () => {
    const h = html();
    expect(h).toContain('role="dialog"');
    expect(h).toContain('aria-modal="true"');
    const id = /aria-labelledby="([^"]+)"/.exec(h)![1];
    expect(h).toContain(`<h2 id="${id}">Checkpoints of x</h2>`);
    expect(h).toContain("inside");
  });
  it("has a Close button, and a dialog that can take focus when it opens", () => {
    const h = html();
    expect(h).toMatch(/<button[^>]*type="button"[^>]*>Close<\/button>/);
    expect(h).toMatch(/class="gm"[^>]*tabindex="-1"|tabindex="-1"[^>]*class="gm"/);
  });
  it("sits on a backdrop that closes it", () => {
    expect(html()).toMatch(/^<div class="gm-scrim"/);
  });
});
