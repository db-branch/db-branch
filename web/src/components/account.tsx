import { FormEvent, useEffect, useRef, useState } from "react";
import { api, isAdmin, managesBases, Session } from "../api";
import { chooseLanguage, language, languages, remember, setLanguage, t } from "../lib/i18n";
import { TokenManager } from "./tokens";
import { ErrorLine, Field } from "./ui";

function roleLabel(roles: string[]): string {
  if (roles.includes("admin") || roles.includes("scope:full")) return t("Administrator");
  if (roles.includes("maintainer")) return t("Maintainer");
  if (roles.includes("developer")) return t("Developer");
  if (roles.includes("viewer")) return t("Viewer");
  return roles.filter((r) => !r.startsWith("scope:"))[0] ?? t("User");
}

export function Avatar({ name }: { name: string }) {
  return (
    <span className="avatar" aria-hidden="true">
      {(name.trim()[0] ?? "?").toUpperCase()}
    </span>
  );
}

// switchLanguage applies a choice ("" follows the browser) and reloads, so
// every view renders in it; signed-in people keep it in their account.
export async function switchLanguage(choice: string, signedIn: boolean) {
  remember(choice);
  if (signedIn) await api.setLanguage(choice).catch(() => {});
  setLanguage(chooseLanguage(choice, navigator.language));
  location.reload();
}

// LanguagePicker is a compact switch for people who are not signed in.
export function LanguagePicker() {
  return (
    <select className="language-picker" aria-label={t("Language")} value={language()} onChange={(e) => switchLanguage(e.target.value, false)}>
      {languages
        .filter(([v]) => v !== "")
        .map(([v, label]) => (
          <option key={v} value={v}>
            {label}
          </option>
        ))}
    </select>
  );
}

// AccountMenu is the signed-in person in the corner: who they are, their
// settings, the other console, signing out.
export function AccountMenu({ session, where, onSignOut }: { session: Session; where: "user" | "admin"; onSignOut: () => void }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const name = session.user.name || session.user.id;
  useEffect(() => {
    if (!open) return;
    const close = (e: Event) => (e.type !== "mousedown" || !ref.current?.contains(e.target as Node)) && setOpen(false);
    const esc = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    window.addEventListener("mousedown", close);
    window.addEventListener("keydown", esc);
    return () => {
      window.removeEventListener("mousedown", close);
      window.removeEventListener("keydown", esc);
    };
  }, [open]);
  return (
    <div className="account" ref={ref}>
      <button className="account-button" aria-haspopup="menu" aria-expanded={open} onClick={() => setOpen(!open)}>
        <Avatar name={name} />
        <span className="account-name">{name}</span>
        <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
          <path d="M2 4l3 3 3-3" fill="none" stroke="currentColor" strokeWidth="1.5" />
        </svg>
      </button>
      {open && (
        <div className="account-menu" role="menu">
          <div className="account-who">
            <Avatar name={name} />
            <div>
              <div className="strong">{name}</div>
              <div className="muted small">{roleLabel(session.user.roles)}</div>
            </div>
          </div>
          <a role="menuitem" href="/account">
            {t("Account settings")}
          </a>
          {where === "user" && managesBases() && (
            <a role="menuitem" href="/admin">
              {isAdmin() ? t("Admin console") : t("Manage bases and users")}
            </a>
          )}
          {where === "admin" && (
            <a role="menuitem" href="/">
              {t("User page")}
            </a>
          )}
          <hr />
          <button role="menuitem" onClick={() => api.signOut(onSignOut)}>
            {t("Sign out")}
          </button>
        </div>
      )}
    </div>
  );
}

// AccountPage shows who is signed in, their language, their MCP tokens,
// and a password change when their account has one here.
export function AccountPage({ session }: { session: Session }) {
  const name = session.user.name || session.user.id;
  return (
    <div className="account-page">
      <h1>{t("Account")}</h1>
      <section className="panel">
        <h2>{t("Profile")}</h2>
        <div className="profile">
          <Avatar name={name} />
          <dl className="profile-facts">
            <dt>{t("Name")}</dt>
            <dd>{name}</dd>
            <dt>{t("Sign-in ID")}</dt>
            <dd className="mono">{session.user.id}</dd>
            <dt>{t("Role")}</dt>
            <dd>{roleLabel(session.user.roles)}</dd>
          </dl>
        </div>
        <p className="muted small">{t("The name and role come from your account; an administrator changes them.")}</p>
      </section>
      <section className="panel">
        <h2>{t("Language")}</h2>
        <div className="segmented small" role="radiogroup" aria-label={t("Language")}>
          {languages.map(([v, label]) => (
            <button
              key={v}
              type="button"
              role="radio"
              aria-checked={(session.language ?? "") === v}
              className={(session.language ?? "") === v ? "on" : ""}
              onClick={() => switchLanguage(v, true)}
            >
              {v === "" ? t(label) : label}
            </button>
          ))}
        </div>
        <p className="muted small">{t("Saved to your account: it follows you to any browser.")}</p>
      </section>
      <TokenManager
        title={t("MCP access")}
        hint={t(
          "Tokens for Claude Code and other MCP clients. They act as you: branches they make are yours, and they can do what your role allows with branches, and no more. Revoke one when a laptop is lost.",
        )}
        list={api.myTokens}
        create={(tok) => api.createMyToken({ name: tok.name, ttl: tok.ttl })}
        revoke={api.revokeMyToken}
        placeholder={t("name, e.g. my-laptop")}
      />
      <section className="panel">
        <h2>{t("Password")}</h2>
        {session.can_change_password ? <PasswordForm /> : <p className="muted">{t("Your password is managed by your sign-in provider, not here.")}</p>}
      </section>
    </div>
  );
}

function PasswordForm() {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [again, setAgain] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);
  const mismatch = again !== "" && again !== next;
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setDone(false);
    try {
      await api.changePassword(current, next);
      setDone(true);
      setError(null);
      setCurrent("");
      setNext("");
      setAgain("");
    } catch (err) {
      setError((err as Error).message);
    }
  };
  return (
    <form className="form narrow" onSubmit={submit}>
      <Field label={t("Current password")}>
        <input type="password" value={current} onChange={(e) => setCurrent(e.target.value)} autoComplete="current-password" />
      </Field>
      <Field label={t("New password")} hint={t("12 characters or more. Your other sessions sign out.")}>
        <input type="password" value={next} onChange={(e) => setNext(e.target.value)} autoComplete="new-password" minLength={12} />
      </Field>
      <Field label={t("New password again")} hint={mismatch ? t("Does not match.") : undefined}>
        <input type="password" value={again} onChange={(e) => setAgain(e.target.value)} autoComplete="new-password" />
      </Field>
      <ErrorLine error={error} />
      {done && <p className="ok-line">{t("Password changed.")}</p>}
      <div className="form-actions">
        <button className="primary" disabled={!current || next.length < 12 || next !== again}>
          {t("Change password")}
        </button>
      </div>
    </form>
  );
}
