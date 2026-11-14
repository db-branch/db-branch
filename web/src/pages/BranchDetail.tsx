import { useContext, useState } from "react";
import { api, can } from "../api";
import { BranchView, BranchSection } from "../components/branchview";
import { CheckpointPanel } from "../components/checkpoints";
import { ConnectPanel } from "../components/connect";
import { UpgradePanel } from "../components/upgrade";
import { BranchHistory } from "../components/branchhistory";
import { hasHistoryModule } from "../lib/history";
import { showDiff } from "../lib/diffpage";
import { useCapabilities } from "../lib/caps";
import { Empty, ErrorLine } from "../components/ui";
import { GoContext } from "../lib/gocontext";
import { showCheckpoints } from "../lib/checkpoints";
import { useLive, useNow } from "../lib/live";
import { t } from "../lib/i18n";
import { ADMIN } from "../lib/nav";
import { DeleteBranch } from "./Branches";

// BranchDetail is the page of one branch (/admin/branches/<name>).
export function BranchDetail({ name }: { name: string }) {
  const go = useContext(GoContext);
  const { data: branches, error } = useLive(api.branches, ["branch", "operation"]);
  const { data: cluster } = useLive(api.cluster, ["node"]);
  const [deleting, setDeleting] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const caps = useCapabilities();
  useNow();
  const back = () => go(`${ADMIN}/branches`);
  const run = (p: Promise<unknown>) => p.then(() => setActionError(null), (e: Error) => setActionError(e.message));
  const b = branches?.find((x) => x.name === name);
  if (!branches) return <section className="page"><ErrorLine error={error} /></section>;
  if (!b) {
    return (
      <section className="page">
        <Empty line={t("No such branch.")} action={<button onClick={back}>{t("Back to Branches")}</button>} />
      </section>
    );
  }
  const ready = b.status === "ready";
  const sections: BranchSection[] = [];
  if (ready) sections.push({ id: "connect", title: t("Connect"), body: <ConnectPanel branch={b.name} /> });
  if (["ready", "restoring"].includes(b.status) && showCheckpoints(b, can)) {
    sections.push({ id: "checkpoints", title: "Checkpoints", body: <CheckpointPanel branch={b} ready={ready} allow={can} /> });
  }
  if (b.record && hasHistoryModule(caps) && can("ext.history.read")) sections.push({ id: "history", title: t("Transactions"), body: <BranchHistory name={b.name} /> });
  if (ready && can("branch.upgrade")) sections.push({ id: "upgrade", title: t("Upgrade"), body: <UpgradePanel branch={b} ready={ready} allow={can} onChange={() => undefined} /> });
  const max = Math.max(1, ...branches.map((x) => x.storage?.referenced ?? 0));
  return (
    <>
      <ErrorLine error={error ?? actionError} />
      <BranchView
        b={b}
        clustered={cluster?.mode === "cluster"}
        max={max}
        sections={sections}
        onBack={back}
        onExtend={(ttl) => run(api.extendBranch(b.name, ttl))}
        onReset={() => run(api.resetBranch(b.name))}
        onRecreate={() => run(api.recreateBranch(b.name))}
        onDelete={() => setDeleting(true)}
        onCompare={showDiff(caps, can) ? () => go(`${ADMIN}/m/diff/ui/?a=${encodeURIComponent(b.name)}`) : undefined}
        onCopyProxy={() => run(api.dsn(b.name, "proxy").then((d) => navigator.clipboard.writeText(d)))}
      />
      {deleting && <DeleteBranch branch={b} onClose={() => setDeleting(false)} onDeleted={back} />}
    </>
  );
}
