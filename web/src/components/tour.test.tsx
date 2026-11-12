import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { TourCard } from "./tour";

const noop = () => {};
const card = (index: number, total = 5) =>
  renderToStaticMarkup(<TourCard id="t" title="Choose a Base" body="Pick one." index={index} total={total} onNext={noop} onBack={noop} onSkip={noop} />);

describe("TourCard", () => {
  it("ダイアログとして名前と説明を結びつける", () => {
    const html = card(0);
    expect(html).toContain('role="dialog"');
    expect(html).toContain('aria-labelledby="t-title"');
    expect(html).toContain('aria-describedby="t-body"');
    expect(html).toContain('id="t-title"');
    expect(html).toContain('id="t-body"');
  });
  it("進み具合を出す", () => {
    expect(card(1)).toContain("2 / 5");
  });
  it("最初は Back がなく、Skip tour と Next がある", () => {
    const html = card(0);
    expect(html).not.toContain(">Back<");
    expect(html).toContain(">Skip tour<");
    expect(html).toContain(">Next<");
  });
  it("途中には Back と Next がある", () => {
    const html = card(2);
    expect(html).toContain(">Back<");
    expect(html).toContain(">Next<");
  });
  it("最後は Done で、Next と Skip tour はない", () => {
    const html = card(4);
    expect(html).toContain(">Done<");
    expect(html).not.toContain(">Next<");
    expect(html).not.toContain(">Skip tour<");
  });
});
