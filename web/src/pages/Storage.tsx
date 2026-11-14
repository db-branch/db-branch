import { useContext, useState } from "react";
import { api } from "../api";
import { StorageView } from "../components/storage";
import { ErrorLine } from "../components/ui";
import { GoContext } from "../lib/gocontext";
import { analyze } from "../lib/storage";
import { useLive, useNow } from "../lib/live";

// Storage is the admin's view of the storage of a node: how much of the pool each
// base and each branch uses.
export function Storage() {
  const go = useContext(GoContext);
  const { data: cluster } = useLive(api.cluster, ["node"]);
  const nodes = cluster ? cluster.nodes.map((n) => n.name) : [];
  const [picked, setPicked] = useState<string | null>(null);
  const node = picked && nodes.includes(picked) ? picked : (cluster?.nodes.find((n) => n.leader)?.name ?? nodes[0] ?? "");
  const { data: report, error } = useLive(() => (node ? api.nodeStorage(node) : Promise.resolve(undefined)), ["base", "branch", "operation", "node", `@${node}`]); // the last entry makes it load again for another node
  const { data: bases } = useLive(api.bases, ["base"]);
  const { data: branches } = useLive(api.branches, ["branch"]);
  useNow();
  const analysis = report && bases && branches ? analyze(report, bases, branches, Date.now()) : null;
  return (
    <>
      <ErrorLine error={error} />
      {analysis && (
        <StorageView
          analysis={analysis}
          nodes={nodes}
          node={node}
          onNode={setPicked}
          onOpen={go}
        />
      )}
    </>
  );
}
