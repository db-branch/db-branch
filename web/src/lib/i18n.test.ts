import { afterEach, describe, expect, it } from "vitest";
import { chooseLanguage, setLanguage, t } from "./i18n";

afterEach(() => setLanguage("en"));

describe("t", () => {
  it("英語のままの文言を選んだ言語に訳し、変数を埋める", () => {
    setLanguage("ja");
    expect(t("Sign in as someone else")).toBe("別のユーザーでサインイン");
    expect(t("Sign in")).toBe("Sign in");
    expect(t("Created {when}.", { when: "3分前" })).toBe("3分前に作成。");
  });
  it("訳がなければ英語を返す", () => {
    setLanguage("ja");
    expect(t("Not translated yet {x}", { x: "1" })).toBe("Not translated yet 1");
    setLanguage("en");
    expect(t("Sign in")).toBe("Sign in");
  });
});

describe("chooseLanguage", () => {
  it("本人の設定、ブラウザの順に決める", () => {
    expect(chooseLanguage("en", "ja-JP")).toBe("en");
    expect(chooseLanguage("", "ja-JP")).toBe("ja");
    expect(chooseLanguage("", "fr-FR")).toBe("en");
    expect(chooseLanguage(undefined, undefined)).toBe("en");
  });
});
