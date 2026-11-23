import { describe, expect, it } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";
import { createElement } from "react";
import { setLanguage } from "../lib/i18n";
import { Banner } from "./Banner";
import { tryLocallyUrl } from "./tryurl";

describe("the 'Run it on your own machine' link", () => {
  it("points an English UI to the English getting-started page, tagged ref=demo", () => {
    expect(tryLocallyUrl("en")).toBe("https://db-branch.com/docs/getting-started/?ref=demo");
  });
  it("points a Japanese UI to the Japanese page", () => {
    expect(tryLocallyUrl("ja")).toBe("https://db-branch.com/ja/docs/getting-started/?ref=demo");
  });
  it("is in the banner, in the language of the UI", () => {
    const html = (lang: "en" | "ja") => {
      setLanguage(lang);
      return renderToStaticMarkup(createElement(Banner, { role: "admin", onSwitch: () => {}, home: "https://db-branch.com" }));
    };
    expect(html("en")).toContain('href="https://db-branch.com/docs/getting-started/?ref=demo"');
    expect(html("en")).toContain("Run it on your own machine");
    expect(html("ja")).toContain('href="https://db-branch.com/ja/docs/getting-started/?ref=demo"');
    expect(html("ja")).toContain("手元で試す");
    setLanguage("en");
  });
});
