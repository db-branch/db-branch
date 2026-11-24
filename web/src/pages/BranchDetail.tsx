import { useContext, useState } from "react";
import { api, type Branch, can } from "../api";
import { BranchView, BranchSection } from "../components/branchview";
import { CheckpointPanel } from "../components/checkpoints";
import { ConnectPanel } from "../components/connect";
import { ForkBranch } from "../components/forkbranch";
import { UpgradePanel } from "../components/upgrade";
import { BranchHistory } from "../components/branchhistory";
import { SqlRunner } from "../components/sqlrunner";
import { engineLabel } from "../lib/format";
import { hasHistoryModule } from "../lib/history";
import { useCapabilities } from "../lib/caps";
import { Empty, ErrorLine, Dialog } from "../components/ui";
import { GoContext } from "../lib/gocontext";
import { showCheckpoints } from "../lib/checkpoints";
import { useLive, useNow } from "../lib/live";
import { t } from "../lib/i18n";
import { ADMIN } from "../lib/nav";
import { DeleteBranch } from "./Branches";

export function ResetBranch({ branch, onClose, onConfirm }: { branch: Branch; onClose: () => void; onConfirm: () => void }) {
  return (
    <Dialog title={t("Reset {name}?", { name: branch.name })} onClose={onClose}>
      <p className="muted">{t("Reset {name} to the data of its Snapshot? Changes you made are lost. Checkpoints are kept.", { name: branch.name })}</p>
      <div className="form-actions">
        <button type="button" onClick={onClose}>
          {t("Cancel")}
        </button>
        <button type="button" className="danger-button" onClick={onConfirm}>
          {t("Confirm reset")}
        </button>
      </div>
    </Dialog>
  );
}

export function createResetHandlers(
  branchName: string,
  setResetting: (open: boolean) => void,
  run: (p: Promise<unknown>) => Promise<unknown>,
  resetFn: (name: string) => Promise<unknown> = api.resetBranch,
) {
  return {
    onReset: () => setResetting(true),
    onClose: () => setResetting(false),
    onConfirm: () => {
      setResetting(false);
      return run(resetFn(branchName));
    },
  };
}

// BranchDetail is the page of one branch (/admin/branches/<name>).
export function BranchDetail({ name }: { name: string }) {
  const go = useContext(GoContext);
  const { data: branches, error } = useLive(api.branches, ["branch", "operation"]);
  const { data: cluster } = useLive(api.cluster, ["node"]);
  const [deleting, setDeleting] = useState(false);
  const [resetting, setResetting] = useState(false);
  const [forking, setForking] = useState<{ checkpoint?: string } | null>(null);
  const [actionError, setActionError] = useState<unknown>(null);
  const caps = useCapabilities();
  useNow();
  const back = () => go(`${ADMIN}/branches`);
  const run = (p: Promise<unknown>) => p.then(() => setActionError(null), (e: unknown) => setActionError(e));
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
  if (ready && can("branch.connect")) sections.push({ id: "connect", title: t("Connect"), body: <ConnectPanel branch={b.name} proxy={b.proxy} /> });
  if (["ready", "restoring"].includes(b.status) && showCheckpoints(b, can)) {
    sections.push({
      id: "checkpoints",
      title: "Checkpoints",
      body: <CheckpointPanel branch={b} ready={ready} allow={can} hideTitle onFork={can("branch.fork") ? (cp) => setForking({ checkpoint: cp }) : undefined} />,
    });
  }
  if (ready && can("branch.query")) sections.push({ id: "sql", title: "SQL", body: <SqlRunner name={b.name} engine={engineLabel(b.runtime).engine} /> });
  if (b.record && hasHistoryModule(caps) && can("ext.history.read")) sections.push({ id: "history", title: t("Transactions"), body: <BranchHistory name={b.name} /> });
  if (ready && can("branch.upgrade")) sections.push({ id: "upgrade", title: t("Upgrade"), body: <UpgradePanel branch={b} ready={ready} allow={can} onChange={() => undefined} /> });
  const resetHandlers = createResetHandlers(b.name, setResetting, run);
  const max = Math.max(1, ...branches.map((x) => x.storage?.referenced ?? 0));
  return (
    <>
      <ErrorLine error={error ?? actionError} />
      <BranchView
        b={b}
        clustered={cluster?.mode === "cluster"}
        max={max}
        sections={sections}
        allow={can}
        onBack={back}
        onExtend={(ttl) => run(api.extendBranch(b.name, ttl))}
        onReset={resetHandlers.onReset}
        onPause={() => run(api.pauseBranch(b.name))}
        onResume={() => run(api.resumeBranch(b.name))}
        onRecreate={() => run(api.recreateBranch(b.name))}
        onDelete={() => setDeleting(true)}
        onFork={can("branch.fork") ? () => setForking({}) : undefined}
        onOpenParent={(parent) => go(`${ADMIN}/branches/${encodeURIComponent(parent)}`)}
        onCopyProxy={() => run(api.dsn(b.name, "proxy").then((d) => navigator.clipboard.writeText(d)))}
      />
      {resetting && (
        <ResetBranch
          branch={b}
          onClose={resetHandlers.onClose}
          onConfirm={resetHandlers.onConfirm}
        />
      )}
      {deleting && <DeleteBranch branch={b} onClose={() => setDeleting(false)} onDeleted={back} />}
      {forking && <ForkBranch branch={b} initialCheckpoint={forking.checkpoint} onClose={() => setForking(null)} onForked={(made) => go(`${ADMIN}/branches/${encodeURIComponent(made)}`)} />}
    </>
  );
}
