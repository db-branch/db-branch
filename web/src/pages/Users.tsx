import { FormEvent, useState } from "react";
import { api, CoreUser, currentSession } from "../api";
import { Icon } from "../components/marks";
import { Dialog, Drawer, Empty, ErrorLine, Field } from "../components/ui";
import { formatDateTime, relative } from "../lib/format";
import { t } from "../lib/i18n";
import { useLive, useNow } from "../lib/live";

export function canDisableOrDeleteUser(target: CoreUser, selfId: string, activeCount: number): boolean {
  if (target.id === selfId) return false;
  if (!target.disabled && activeCount <= 1) return false;
  return true;
}

export function Users() {
  const { data: users, error, reload } = useLive(api.users, ["user"]);
  const [adding, setAdding] = useState(false);
  const [shownPassword, setShownPassword] = useState<{ title: string; id: string; password: string } | null>(null);
  const [resetTarget, setResetTarget] = useState<CoreUser | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<CoreUser | null>(null);
  const [actionError, setActionError] = useState<unknown>(null);
  useNow();

  const selfId = currentSession()?.user.id ?? "";
  const list = users ?? [];
  const activeCount = list.filter((u) => !u.disabled).length;

  const toggleDisabled = async (u: CoreUser) => {
    setActionError(null);
    try {
      await api.updateUser(u.id, { disabled: !u.disabled });
      reload();
    } catch (err) {
      setActionError(err);
    }
  };

  return (
    <section className="page">
      <header className="page-head">
        <h1>{t("Users")}</h1>
        <button type="button" className="primary" onClick={() => setAdding(true)}>
          <Icon name="plus" /> User
        </button>
      </header>
      <p className="muted">
        {t("Console users who can sign in and manage bases and branches.")}
      </p>
      <ErrorLine error={error ?? actionError} />
      {users && list.length === 0 && <Empty line={t("No users.")} />}
      {list.length > 0 && (
        <UsersTable
          users={list}
          selfId={selfId}
          activeCount={activeCount}
          onToggleDisabled={toggleDisabled}
          onResetPassword={setResetTarget}
          onDelete={setDeleteTarget}
        />
      )}
      {adding && (
        <AddUserDrawer
          onClose={() => setAdding(false)}
          onCreated={(u, initialPassword) => {
            setAdding(false);
            reload();
            if (initialPassword) {
              setShownPassword({ title: t("Initial password"), id: u.id, password: initialPassword });
            }
          }}
        />
      )}
      {resetTarget && (
        <ResetPasswordDialog
          user={resetTarget}
          onClose={() => setResetTarget(null)}
          onReset={(pw) => {
            const id = resetTarget.id;
            setResetTarget(null);
            reload();
            setShownPassword({ title: t("New password"), id, password: pw });
          }}
        />
      )}
      {deleteTarget && (
        <DeleteUserDialog
          user={deleteTarget}
          onClose={() => setDeleteTarget(null)}
          onDeleted={() => {
            setDeleteTarget(null);
            reload();
          }}
        />
      )}
      {shownPassword && (
        <OneTimePasswordDialog
          title={shownPassword.title}
          userId={shownPassword.id}
          password={shownPassword.password}
          onClose={() => setShownPassword(null)}
        />
      )}
    </section>
  );
}

export function UsersTable({
  users,
  selfId,
  activeCount,
  now = Date.now(),
  onToggleDisabled,
  onResetPassword,
  onDelete,
}: {
  users: CoreUser[];
  selfId: string;
  activeCount: number;
  now?: number;
  onToggleDisabled: (u: CoreUser) => void;
  onResetPassword: (u: CoreUser) => void;
  onDelete: (u: CoreUser) => void;
}) {
  return (
    <div className="table-wrap">
      <table className="rows">
        <thead>
          <tr>
            <th>{t("User")}</th>
            <th>{t("Email")}</th>
            <th>{t("Status")}</th>
            <th>{t("Created")}</th>
            <th aria-label={t("Actions")} />
          </tr>
        </thead>
        <tbody>
          {users.map((u) => {
            const allowed = canDisableOrDeleteUser(u, selfId, activeCount);
            const isSelf = u.id === selfId;
            return (
              <tr key={u.id}>
                <td>
                  <strong>{u.name}</strong>
                  {isSelf && <span className="badge">{t("you")}</span>}
                  {u.kind === "admin" && <span className="badge">admin</span>}
                  <div className="muted small mono">{u.id}</div>
                </td>
                <td className="mono">{u.email || "—"}</td>
                <td>
                  <span className={`badge${u.disabled ? " warn" : ""}`}>
                    {u.disabled ? t("Disabled") : t("Active")}
                  </span>
                  {u.must_change_password && (
                    <span className="badge">{t("Password change required")}</span>
                  )}
                </td>
                <td>
                  {u.created_at ? (
                    <span className="mono muted" title={formatDateTime(u.created_at)}>
                      {relative(u.created_at, now)}
                    </span>
                  ) : (
                    <span className="muted">—</span>
                  )}
                </td>
                <td className="row-actions">
                  <button type="button" onClick={() => onResetPassword(u)}>
                    {t("Reset password")}
                  </button>
                  <button type="button" disabled={!u.disabled && !allowed} onClick={() => onToggleDisabled(u)}>
                    {u.disabled ? t("Enable") : t("Disable")}
                  </button>
                  <button type="button" className="danger-button" disabled={!allowed} onClick={() => onDelete(u)}>
                    {t("Delete")}
                  </button>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function AddUserDrawer({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  onCreated: (u: CoreUser, initialPassword?: string) => void;
}) {
  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [mustChange, setMustChange] = useState<boolean | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const effectiveMustChange = mustChange ?? password === "";

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const res = await api.createUser({
        email: email.trim(),
        name: name.trim() || undefined,
        password: password || undefined,
        must_change_password: effectiveMustChange,
      });
      onCreated(res.user, res.initial_password);
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };

  return (
    <Drawer title={t("Add user")} onClose={onClose}>
      <form className="form" onSubmit={submit}>
        <Field label={t("Email")}>
          <input type="email" className="mono" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="alice@example.com" autoFocus required />
        </Field>
        <Field label={t("Name")} hint={t("Display name (optional; defaults to email).")}>
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="Alice" maxLength={120} />
        </Field>
        <Field label={t("Password")} hint={t("Leave empty to generate a random initial password (shown once).")}>
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} minLength={password ? 12 : undefined} autoComplete="new-password" />
        </Field>
        <label className="check">
          <input type="checkbox" checked={effectiveMustChange} onChange={(e) => setMustChange(e.target.checked)} />
          <span>{t("Require password change at first sign-in")}</span>
        </label>
        <ErrorLine error={error} />
        <div className="form-actions">
          <button type="button" onClick={onClose}>
            {t("Cancel")}
          </button>
          <button type="submit" className="primary" disabled={busy || !email.trim()}>
            {t("Add")}
          </button>
        </div>
      </form>
    </Drawer>
  );
}

export function ResetPasswordDialog({
  user,
  onClose,
  onReset,
}: {
  user: CoreUser;
  onClose: () => void;
  onReset: (password: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  return (
    <Dialog title={t("Reset password for {id}?", { id: user.email || user.id })} onClose={onClose}>
      <p className="muted">
        {t("Generates a new random password, signs the user out of existing sessions, and requires a password change on next sign-in.")}
      </p>
      <ErrorLine error={error} />
      <div className="form-actions">
        <button type="button" onClick={onClose}>
          {t("Cancel")}
        </button>
        <button
          type="button"
          className="primary"
          disabled={busy}
          onClick={async () => {
            setBusy(true);
            try {
              const res = await api.resetUserPassword(user.id, true);
              onReset(res.password);
            } catch (err) {
              setError(err);
              setBusy(false);
            }
          }}
        >
          {t("Confirm reset")}
        </button>
      </div>
    </Dialog>
  );
}

export function DeleteUserDialog({
  user,
  onClose,
  onDeleted,
}: {
  user: CoreUser;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  return (
    <Dialog title={t("Delete {name}?", { name: user.email || user.id })} onClose={onClose}>
      <p className="muted">
        {t("Removes the user account and revokes all active sessions and personal API tokens.")}
      </p>
      <ErrorLine error={error} />
      <div className="form-actions">
        <button type="button" onClick={onClose}>
          {t("Cancel")}
        </button>
        <button
          type="button"
          className="danger-button"
          disabled={busy}
          onClick={async () => {
            setBusy(true);
            try {
              await api.deleteUser(user.id);
              onDeleted();
            } catch (err) {
              setError(err);
              setBusy(false);
            }
          }}
        >
          {t("Confirm delete")}
        </button>
      </div>
    </Dialog>
  );
}

export function OneTimePasswordDialog({
  title,
  userId,
  password,
  onClose,
}: {
  title: string;
  userId: string;
  password: string;
  onClose: () => void;
}) {
  const [copied, setCopied] = useState(false);
  return (
    <Dialog title={title} onClose={onClose}>
      <p className="muted">
        {t("Shown only once for {id}. Share it securely with the user.", { id: userId })}
      </p>
      <div className="secret-box mono">{password}</div>
      <div className="form-actions">
        <button
          type="button"
          onClick={() => {
            navigator.clipboard?.writeText(password).then(() => setCopied(true), () => {});
          }}
        >
          {copied ? t("Copied") : t("Copy")}
        </button>
        <button type="button" className="primary" onClick={onClose}>
          {t("Done")}
        </button>
      </div>
    </Dialog>
  );
}
