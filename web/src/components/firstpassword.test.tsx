import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
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
