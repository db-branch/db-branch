import { apiHost } from "../lib/host";
import { FormEvent, useState } from "react";
import { APIToken } from "../api";
import { t } from "../lib/i18n";
import { useLive } from "../lib/live";
import { CopyButton } from "./marks";
import { ErrorLine } from "./ui";

type Props = {
  title: string;
  hint: React.ReactNode;
  list: () => Promise<APIToken[]>;
  create: (t: { name: string; scope?: string; ttl?: string }) => Promise<APIToken>;
  revoke: (id: string) => Promise<void>;
  // withScope: service tokens choose branches or full; personal ones are
  // always branches and act as their owner.
  withScope?: boolean;
  placeholder: string;
};

// TokenManager lists, issues (shown once, with the MCP command) and revokes
// tokens: the service tokens in Settings, a person's own in Account.
export function TokenManager({ title, hint, list, create, revoke, withScope, placeholder }: Props) {
  const { data: tokens, reload } = useLive(list, ["token"]);
  const [name, setName] = useState("");
  const [scope, setScope] = useState("branches");
  const [ttl, setTTL] = useState("720h");
  const [issued, setIssued] = useState<APIToken | null>(null);
  const [error, setError] = useState<string | null>(null);
  const run = (p: Promise<unknown>) => p.then(() => (setError(null), reload()), (e: Error) => setError(e.message));
  const endpoint = `https://${apiHost()}/v1/mcp`;
  const command = issued ? `claude mcp add --transport http db-branch ${endpoint} --header "Authorization: Bearer ${issued.token}"` : "";

  const submit = (e: FormEvent) => {
    e.preventDefault();
    run(create({ name, scope: withScope ? scope : undefined, ttl: ttl || undefined }).then((t) => (setIssued(t), setName(""))));
  };
  return (
    <section className="settings-card">
      <h2 className="section-title">{title}</h2>
      <p className="field-hint">{hint}</p>
      {tokens && tokens.length === 0 && <p className="muted small">{t("None yet.")}</p>}
      <table className="rows tokens">
        <tbody>
          {(tokens ?? []).map((tok) => (
            <tr key={tok.id}>
              <td className="mono strong">{tok.name}</td>
              {withScope && <td className="mono muted">{tok.scope}</td>}
              <td className="muted">{tok.expires_at ? t("until {date}", { date: new Date(tok.expires_at).toLocaleDateString() }) : t("no expiry")}</td>
              <td className="col-actions">
                <button onClick={() => run(revoke(tok.id))}>{t("Revoke")}</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <form className="token-form" onSubmit={submit}>
        <input className="mono" value={name} onChange={(e) => setName(e.target.value)} placeholder={placeholder} required maxLength={64} />
        {withScope && (
          <select value={scope} onChange={(e) => setScope(e.target.value)} aria-label={t("Scope")}>
            <option value="branches">branches</option>
            <option value="full">full</option>
          </select>
        )}
        <select value={ttl} onChange={(e) => setTTL(e.target.value)} aria-label={t("Expires after")}>
          <option value="168h">{t("7 days")}</option>
          <option value="720h">{t("30 days")}</option>
          <option value="2160h">{t("90 days")}</option>
          <option value="8760h">{t("1 year")}</option>
          <option value="">{t("No expiry")}</option>
        </select>
        <button className="primary" disabled={!name}>
          {t("Issue")}
        </button>
      </form>
      <ErrorLine error={error} />
      {issued && (
        <div className="issued">
          <p>
            {t("Token")} <span className="mono strong">{issued.name}</span>
            {t(", shown only now:")}
          </p>
          <code className="mono">{issued.token}</code> <CopyButton label={t("Copy token")} load={async () => issued.token ?? ""} />
          <p className="field-hint">{t("Add it to Claude Code (MCP):")}</p>
          <code className="mono">{command}</code> <CopyButton label={t("Copy command")} load={async () => command} />
          <p className="field-hint">
            {t("The node's certificate is self-signed: save")} <a href="/tls.pem">tls.pem</a> {t("and point NODE_EXTRA_CA_CERTS at it before starting the client.")}
          </p>
        </div>
      )}
    </section>
  );
}
