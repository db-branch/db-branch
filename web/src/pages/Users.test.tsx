import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it } from "vitest";
import type { CoreUser } from "../api";
import { setLanguage } from "../lib/i18n";
import { usersRedirect } from "../lib/nav";
import { OneTimePasswordDialog, UsersTable, canDisableOrDeleteUser } from "./Users";

afterEach(() => setLanguage("en"));

const user = (over: Partial<CoreUser> = {}): CoreUser => ({
  id: "user:alice@example.com",
  email: "alice@example.com",
  name: "Alice",
  kind: "user",
  disabled: false,
  must_change_password: false,
  created_at: "2026-11-26T05:00:00Z",
  updated_at: "2026-11-26T05:00:00Z",
  ...over,
});

describe("Users page", () => {
  it("protects self and last active user from disable or delete", () => {
    const admin = user({ id: "admin", email: "", name: "admin", kind: "admin" });
    const alice = user();
    const disabledBob = user({ id: "user:bob@example.com", email: "bob@example.com", name: "Bob", disabled: true });

    expect(canDisableOrDeleteUser(admin, "admin", 2)).toBe(false);
    expect(canDisableOrDeleteUser(alice, "admin", 2)).toBe(true);
    expect(canDisableOrDeleteUser(alice, "admin", 1)).toBe(false);
    expect(canDisableOrDeleteUser(disabledBob, "admin", 1)).toBe(true);
  });

  it("renders table, one-time password dialog, and Japanese labels cleanly", () => {
    const html = renderToStaticMarkup(
      <UsersTable
        users={[
          user({ id: "admin", email: "", name: "admin", kind: "admin" }),
          user({ must_change_password: true }),
          user({ id: "user:bob@example.com", email: "bob@example.com", name: "Bob", disabled: true }),
        ]}
        selfId="admin"
        activeCount={2}
        now={Date.parse("2026-11-26T08:00:00Z")}
        onToggleDisabled={() => {}}
        onResetPassword={() => {}}
        onDelete={() => {}}
      />,
    );
    expect(html).toContain("alice@example.com");
    expect(html).toContain("Password change required");
    expect(html).toContain("Disabled");

    setLanguage("ja");
    const jaDlg = renderToStaticMarkup(
      <OneTimePasswordDialog title="Initial password" userId="user:alice@example.com" password="secret-password-24chars!" onClose={() => {}} />,
    );
    expect(jaDlg).toContain("user:alice@example.com 用のパスワードです");
    expect(jaDlg).toContain("secret-password-24chars!");
  });

  it("redirects /admin/users to auth module page when auth module is active", () => {
    expect(usersRedirect("/admin/users", [{ module: "auth", path: "/ui/", title: "Users" }])).toBe("/admin/m/auth/ui/");
    expect(usersRedirect("/admin/users", [{ module: "audit", path: "/ui/", title: "Audit" }])).toBeNull();
    expect(usersRedirect("/admin/branches", [{ module: "auth", path: "/ui/", title: "Users" }])).toBeNull();
  });
});
