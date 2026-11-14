import { useCallback, useState } from "react";
import { FilterValues, parseFilter, toSearch } from "./filter";

// useFilter holds the search and filters of a list and keeps them in the
// address (?q=…&status=…), so a reload or a shared link shows the same list.
export function useFilter(keys: string[]): [FilterValues, (v: FilterValues) => void] {
  const [values, set] = useState<FilterValues>(() => parseFilter(location.search, keys));
  const update = useCallback((v: FilterValues) => {
    set(v);
    try {
      history.replaceState(null, "", location.pathname + toSearch(v) + location.hash);
    } catch {
      /* the address is only a convenience */
    }
  }, []);
  return [values, update];
}
