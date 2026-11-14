import { splitOwned } from "../lib/branchname";

// BranchName shows a branch name. A name that starts with its owner's prefix
// has that prefix muted; otherwise the owner is shown muted before the name.
export function BranchName({ name, owner }: { name: string; owner?: string | null }) {
  const [prefix, rest] = splitOwned(name, owner ?? undefined);
  if (prefix) {
    return (
      <span className="mono strong" title={owner ?? undefined}>
        <span className="muted">{prefix}</span>
        {rest}
      </span>
    );
  }
  return (
    <>
      {owner && (
        <span className="mono muted" title={owner}>
          {owner.split("@")[0]}/
        </span>
      )}
      <span className="mono strong">{name}</span>
    </>
  );
}
