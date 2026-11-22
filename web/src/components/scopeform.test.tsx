import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { SettingScope } from "../api";
import { ScopeForm, Secret } from "./scopeform";

const scope = (secretSet: boolean): SettingScope => ({
  scope: "core",
  title: "Core",
  settings: [
    { key: "license.key", label: "License key", type: "secret", default: "", value: "", secret_set: secretSet },
    { key: "note", label: "Note", type: "text", default: "", value: "line\n " },
  ],
});

describe("ScopeForm", () => {
  it("設定済みの secret は値を出さず「Set」と表示し、Clear を出す", () => {
    const h = renderToStaticMarkup(<ScopeForm scope={scope(true)} />);
    expect(h).toMatch(/placeholder="Set"/);
    expect(h).toMatch(/>Clear</);
    expect(h).not.toMatch(/<input[^>]*type="password"[^>]*value="[^"]/);
  });
  it("未設定の secret には Clear を出さない", () => {
    const h = renderToStaticMarkup(<ScopeForm scope={scope(false)} />);
    expect(h).not.toMatch(/>Clear</);
    expect(h).toMatch(/placeholder="none"/);
  });
  it("text型は末尾の改行とスペースを消さずに表示する", () => {
    expect(renderToStaticMarkup(<ScopeForm scope={scope(false)} />)).toContain("line\n ");
  });
  it("Clear を押した secret は「Will be cleared」と表示する", () => {
    const h = renderToStaticMarkup(<Secret value="" placeholder="Set" onChange={() => {}} clearable clearing onClear={() => {}} />);
    expect(h).toContain("Will be cleared");
  });
});
