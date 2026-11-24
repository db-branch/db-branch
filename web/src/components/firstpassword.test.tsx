import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Session } from "../api";
import { nextSessionRoute } from "../App";
import { nextPortalSessionAction } from "../pages/Portal";
import { FirstPasswordView } from "./firstpassword";

const noop = () => {};
const view = (props: Record<string, unknown> = {}) =>
  renderToStaticMarkup(<FirstPasswordView next="" again="" onNext={noop} onAgain={noop} onSubmit={noop} error={null} {...props} />);

describe("FirstPasswordView", () => {
  it("asks for a new password twice and has a Set password button", () => {
    const h = view();
    expect(h).toContain("New password");
    expect(h).toContain("New password again");
    expect(h).toMatch(/<input[^>]*type="password"[^>]*autoComplete="new-password"/);
    expect(h).toMatch(/<button[^>]*>Set password</);
    expect(h).toContain("12 characters or more.");
  });
  it("keeps the button disabled until the password is 12 characters or more and typed twice the same", () => {
    expect(view()).toMatch(/<button[^>]*disabled[^>]*>Set password/);
    expect(view({ next: "short", again: "short" })).toMatch(/<button[^>]*disabled[^>]*>Set password/);
    expect(view({ next: "a much better passphrase", again: "a much better passphrasX" })).toMatch(/<button[^>]*disabled[^>]*>Set password/);
    expect(view({ next: "a much better passphrase", again: "a much better passphrase" })).not.toMatch(/<button[^>]*disabled[^>]*>Set password/);
  });
  it("says when the two do not match, and shows an error from the server", () => {
    expect(view({ next: "a much better passphrase", again: "different" })).toContain("Does not match.");
    expect(view({ error: "password must be at least 12 characters" })).toContain("password must be at least 12 characters");
  });
});

describe("TestFirstPassword_mustChangePasswordリロード時にPortal含めパスワード変更画面を維持する", () => {
  const mustChangeSess: Session = { user: { id: "admin", name: "admin", roles: ["admin"] }, csrf_token: "c", must_change_password: true };
  const readySess: Session = { user: { id: "admin", name: "admin", roles: ["admin"] }, csrf_token: "c", must_change_password: false };

  it("App と Portal の両方で must_change_password=true ならパスワード変更画面に遷移し、無印ログインや匿名扱いにしない", () => {
    expect(nextSessionRoute(mustChangeSess)).toBe("first_password");
    expect(nextSessionRoute(readySess)).toBe("in");
    expect(nextSessionRoute(null)).toBe("login");

    expect(nextPortalSessionAction(mustChangeSess)).toBe("first_password");
    expect(nextPortalSessionAction(readySess)).toBe("in");
    expect(nextPortalSessionAction(null)).toBe("anonymous");
  });

  it("リロード後に現在パスワードを持たない場合は Current password 入力欄と Sign out ボタンを出す", () => {
    const h = view({ needCurrent: true, current: "", onCurrent: noop, onSignOut: noop });
    expect(h).toContain("Current password");
    expect(h).toContain("Sign out");
    expect(h).toMatch(/<button[^>]*disabled[^>]*>Set password/);
    const filled = view({ needCurrent: true, current: "initial-pass", next: "a much better passphrase", again: "a much better passphrase", onCurrent: noop });
    expect(filled).not.toMatch(/<button[^>]*disabled[^>]*>Set password/);
  });
});
