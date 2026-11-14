import { useContext, useState } from "react";
import { api } from "../api";
import { Engine, Status } from "../components/marks";
import { Empty, ErrorLine } from "../components/ui";
import { GoContext } from "../lib/gocontext";
import { useLive, useNow } from "../lib/live";
import { t } from "../lib/i18n";
import { ADMIN } from "../lib/nav";
import { BaseBody, DeleteBase, UpdateData } from "./Bases";

// BaseDetailPage is the page of one base (/admin/bases/<name>): what the drawer
// of the list used to hold, as a page to grow.
export function BaseDetailPage({ name }: { name: string }) {
  const go = useContext(GoContext);
  const { data: bases, error } = useLive(api.bases, ["base", "operation"]);
  const { data: branches } = useLive(api.branches, ["branch"]);
  const { data: cluster } = useLive(api.cluster, ["node"]);
  const [updating, setUpdating] = useState(false);
  const [deleting, setDeleting] = useState(false);
  useNow();
  const back = () => go(`${ADMIN}/bases`);
  const base = bases?.find((x) => x.name === name);
  if (!bases) {
    return (
      <section className="page">
        <ErrorLine error={error} />
      </section>
    );
  }
  if (!base) {
    return (
      <section className="page">
        <Empty line={t("No such base.")} action={<button onClick={back}>{t("Back to Bases")}</button>} />
      </section>
    );
  }
  const nodes = cluster?.mode === "cluster" ? cluster.nodes.map((n) => n.name) : [];
  const used = (branches ?? []).filter((x) => x.base === base.name);
  return (
    <section className="page base-page">
      <a
        className="text-button back-link"
        href={`${ADMIN}/bases`}
        onClick={(e) => {
          e.preventDefault();
          back();
        }}
      >
        ← {t("Back to Bases")}
      </a>
      <header className="page-head">
        <h1 className="mono">{base.name}</h1>
        <Status status={base.status} reason={base.status_reason} />
        <Engine runtime={base.runtime} />
      </header>
      <ErrorLine error={error} />
      <BaseBody base={base} branches={used} nodes={nodes} onUpdate={() => setUpdating(true)} onDelete={() => setDeleting(true)} />
      {updating && <UpdateData base={base} onClose={() => setUpdating(false)} />}
      {deleting && <DeleteBase base={base} branches={used.length} onClose={() => setDeleting(false)} onDeleted={back} />}
    </section>
  );
}
