import { FormEvent, useState } from "react";
import { api, Session } from "../api";
import { t } from "../lib/i18n";
import { ErrorLine, Field } from "./ui";

export const MIN_PASSWORD = 12;

// FirstPasswordView: the page shown after signing in with an initial password
// (init --evaluation). Nothing else works until a new one is set.
export function FirstPasswordView({
  next,
  again,
  onNext,
  onAgain,
  onSubmit,
  error,
}: {
  next: string;
  again: string;
  onNext: (v: string) => void;
  onAgain: (v: string) => void;
  onSubmit: (e: FormEvent) => void;
  error: string | null;
}) {
  const mismatch = again !== "" && again !== next;
  return (
    <form className="login-box" onSubmit={onSubmit}>
      <Field label={t("New password")} hint={t("12 characters or more.")}>
        <input type="password" value={next} onChange={(e) => onNext(e.target.value)} autoFocus autoComplete="new-password" minLength={MIN_PASSWORD} />
      </Field>
      <Field label={t("New password again")} hint={mismatch ? t("Does not match.") : undefined}>
        <input type="password" value={again} onChange={(e) => onAgain(e.target.value)} autoComplete="new-password" />
      </Field>
      <ErrorLine error={error} />
      <button className="primary wide" disabled={next.length < MIN_PASSWORD || next !== again}>
        {t("Set password")}
      </button>
    </form>
  );
}

// FirstPassword changes the password that was just used to sign in, then
// hands over the refreshed session.
export function FirstPassword({ current, onDone }: { current: string; onDone: (s: Session) => void }) {
  const [next, setNext] = useState("");
  const [again, setAgain] = useState("");
  const [error, setError] = useState<string | null>(null);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await api.changePassword(current, next);
      onDone(await api.session());
    } catch (err) {
      setError((err as Error).message);
    }
  };
  return <FirstPasswordView next={next} again={again} onNext={setNext} onAgain={setAgain} onSubmit={submit} error={error} />;
}
