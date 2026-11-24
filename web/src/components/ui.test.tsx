import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it } from "vitest";
import { ApiError } from "../api";
import { setLanguage } from "../lib/i18n";
import { ErrorLine } from "./ui";

afterEach(() => setLanguage("en"));

describe("ErrorLine", () => {
  it("renders nothing when error is null", () => {
    expect(renderToStaticMarkup(<ErrorLine error={null} />)).toBe("");
  });

  it("renders original message in en mode", () => {
    setLanguage("en");
    const html = renderToStaticMarkup(<ErrorLine error={new ApiError(409, "already_exists", "branch x already exists")} />);
    expect(html).toContain("branch x already exists");
  });

  it("translates known ApiError codes in ja mode without raw English text", () => {
    setLanguage("ja");
    const html = renderToStaticMarkup(<ErrorLine error={new ApiError(409, "already_exists", "branch x already exists")} />);
    expect(html).toContain("同じ名前のリソースがすでに存在します。");
    expect(html).not.toContain("branch x already exists");
  });

  it("renders generic Japanese message and small original text for unknown codes in ja mode", () => {
    setLanguage("ja");
    const html = renderToStaticMarkup(<ErrorLine error={new ApiError(500, "unexpected_code", "raw detail")} />);
    expect(html).toContain("エラーが発生しました。");
    expect(html).toContain('<small class="error-detail">raw detail</small>');
  });

  it("appends optional note after the translated error", () => {
    setLanguage("ja");
    const html = renderToStaticMarkup(<ErrorLine error={new ApiError(422, "invalid_license", "bad sig")} note="キーは保存されませんでした。" />);
    expect(html).toContain("ライセンスキーが無効です。 キーは保存されませんでした。");
    expect(html).not.toContain("bad sig");
  });
});
