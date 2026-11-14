import { useEffect, useState } from "react";
import { api, HistoryTx } from "../api";
import { TxFilter } from "../lib/history";
import { TxTableView } from "./txtable";

const REFRESH_MS = 15000;
const PAGE = 100;

// BranchHistory loads the transactions of the branch's live line (newest first,
// refreshed now and then) and lets the person read further back.
export function BranchHistory({ name }: { name: string }) {
  const [line, setLine] = useState<number | null>(null);
  const [newest, setNewest] = useState<HistoryTx[]>([]);
  const [older, setOlder] = useState<HistoryTx[]>([]);
  const [cursor, setCursor] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [filter, setFilter] = useState<TxFilter>({ text: "", table: "" });

  useEffect(() => {
    let live = true;
    const load = async () => {
      try {
        const g = await api.checkpointGraph(name);
        const r = await api.historyEvents(name, g.live, PAGE);
        if (!live) return;
        setError(null);
        setLine((prev) => {
          if (prev !== g.live) setOlder([]);
          return g.live;
        });
        setNewest(r.items);
        // Keep the cursor of what was read further back; the first page's own cursor only before any.
        setCursor((c) => (c === null || c === undefined ? r.next_before : c));
      } catch (e) {
        if (live) setError((e as Error).message);
      }
    };
    void load();
    const id = setInterval(load, REFRESH_MS);
    return () => {
      live = false;
      clearInterval(id);
    };
  }, [name]);

  const more = async () => {
    if (line === null || !cursor) return;
    setLoading(true);
    try {
      const r = await api.historyEvents(name, line, PAGE, cursor);
      setOlder((o) => [...o, ...r.items]);
      setCursor(r.next_before);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  };
  // The first page may have moved on since older ones were read: show each transaction once.
  const seen = new Set(newest.map((x) => x.gtid));
  const items = [...newest, ...older.filter((x) => !seen.has(x.gtid))];
  return (
    <>
      {error && <p className="field-error">{error}</p>}
      <TxTableView items={items} filter={filter} onFilter={setFilter} more={!!cursor} loading={loading} onMore={() => void more()} line={line ?? 0} />
    </>
  );
}
