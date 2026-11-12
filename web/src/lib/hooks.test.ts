import { afterEach, describe, expect, it } from "vitest";
import { hooks, moduleFrame } from "./hooks";

afterEach(() => {
  delete hooks.frame;
  delete hooks.loginHint;
});

describe("moduleFrame", () => {
  it("loads the module page from the agent unless a hook says otherwise", () => {
    expect(moduleFrame("auth", "/users", "ja")).toEqual({ src: "/v1/ext/auth/users?lang=ja" });
  });
  it("lets the demo build supply the page itself", () => {
    hooks.frame = (m, p, l) => ({ srcDoc: `${m}${p}${l}` });
    expect(moduleFrame("auth", "/users", "en")).toEqual({ srcDoc: "auth/usersen" });
  });
});
