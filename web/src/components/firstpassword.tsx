import { FormEvent, useState } from "react";
import { api, Session } from "../api";
import { t } from "../lib/i18n";
import { ErrorLine, Field } from "./ui";

export const MIN_PASSWORD = 12;

// FirstPasswordView: the page shown after signing in with an initial password
// (init --evaluation). Nothing else works until a new one is set.
export function FirstPasswordView({
  needCurrent = false,
  current = "",
  onCurrent,
  next,
  again,
  onNext,
  onAgain,
  onSubmit,
  onSignOut,
  error,
}: {
  needCurrent?: boolean;
  current?: string;
  onCurrent?: (v: string) => void;
  next: string;
  again: string;
  onNext: (v: string) => void;
  onAgain: (v: string) => void;
  onSubmit: (e: FormEvent) => void;
  onSignOut?: () => void;
  error: string | null;
}) {
  const mismatch = again !== "" && again !== next;
  const incomplete = (needCurrent && !current) || next.length < MIN_PASSWORD || next !== again;
  return (
    <form className="login-box" onSubmit={onSubmit}>
      {needCurrent && (
        <Field label={t("Current password")}>
          <input type="password" value={current} onChange={(e) => onCurrent?.(e.target.value)} autoFocus autoComplete="current-password" />
        </Field>
      )}
      <Field label={t("New password")} hint={t("12 characters or more.")}>
        <input type="password" value={next} onChange={(e) => onNext(e.target.value)} autoFocus={!needCurrent} autoComplete="new-password" minLength={MIN_PASSWORD} />
      </Field>
      <Field label={t("New password again")} hint={mismatch ? t("Does not match.") : undefined}>
        <input type="password" value={again} onChange={(e) => onAgain(e.target.value)} autoComplete="new-password" />
      </Field>
      <ErrorLine error={error} />
      <button className="primary wide" disabled={incomplete}>
        {t("Set password")}
      </button>
      {onSignOut && (
        <button type="button" className="text-button" onClick={onSignOut}>
          {t("Sign out")}
        </button>
      )}
    </form>
  );
}

// FirstPassword changes the password that was just used to sign in (or asks
// for it again after a page reload), then hands over the refreshed session.
export function FirstPassword({ current, onDone, onSignOut }: { current?: string; onDone: (s: Session) => void; onSignOut?: () => void }) {
  const needCurrent = current === undefined;
  const [typedCurrent, setTypedCurrent] = useState("");
  const [next, setNext] = useState("");
  const [again, setAgain] = useState("");
  const [error, setError] = useState<string | null>(null);
  const currentPassword = needCurrent ? typedCurrent : current;
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await api.changePassword(currentPassword, next);
      onDone(await api.session());
    } catch (err) {
      setError((err as Error).message);
    }
  };
  return (
    <FirstPasswordView
      needCurrent={needCurrent}
      current={typedCurrent}
      onCurrent={setTypedCurrent}
      next={next}
      again={again}
      onNext={setNext}
      onAgain={setAgain}
      onSubmit={submit}
      onSignOut={onSignOut}
      error={error}
    />
  );
}
