import { describe, expect, it } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";
import { createElement } from "react";
import { setLanguage } from "../lib/i18n";
import { Banner } from "./Banner";
import { interestUrl, tryLocallyUrl } from "./tryurl";

describe("the 'Run it on your own machine' link", () => {
  // The link goes through a path of the demo's own (see demo-worker/_redirects), because the site's analytics count paths, not query strings.
  it("points an English UI to the counting path for the English getting-started page", () => {
    expect(tryLocallyUrl("en")).toBe("/go/getting-started");
  });
  it("points a Japanese UI to the Japanese one", () => {
    expect(tryLocallyUrl("ja")).toBe("/go/getting-started-ja");
  });
  it("is in the banner, in the language of the UI", () => {
    const html = (lang: "en" | "ja") => {
      setLanguage(lang);
      return renderToStaticMarkup(createElement(Banner, { role: "admin", onSwitch: () => {}, home: "https://db-branch.com" }));
    };
    expect(html("en")).toContain('href="/go/getting-started"');
    expect(html("en")).toContain("Run it on your own machine");
    expect(html("ja")).toContain('href="/go/getting-started-ja"');
    expect(html("ja")).toContain("手元で試す");
    setLanguage("en");
  });
});

describe("the 'Interested in Team / Business?' link", () => {
  it("goes through the counting path for the pricing page, in the language of the UI", () => {
    expect(interestUrl("en")).toBe("/go/contact");
    expect(interestUrl("ja")).toBe("/go/contact-ja");
  });
  it("is in the banner next to the 'run it' button, in both languages", () => {
    const html = (lang: "en" | "ja") => {
      setLanguage(lang);
      return renderToStaticMarkup(createElement(Banner, { role: "admin", onSwitch: () => {}, home: "https://db-branch.com" }));
    };
    expect(html("en")).toContain('href="/go/contact"');
    expect(html("en")).toContain("Interested in Team / Business?");
    expect(html("ja")).toContain('href="/go/contact-ja"');
    expect(html("ja")).toContain("Team / Business に興味がありますか？");
    setLanguage("en");
  });
});
