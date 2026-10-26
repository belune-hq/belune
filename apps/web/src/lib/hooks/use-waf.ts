import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { queryKeys } from "./query-keys";
import * as wafApi from "@/lib/api/waf";
import type { WafEventFilters } from "@/lib/api/waf";
import type { WafWindow } from "@/lib/types";

export function useWafStatus(window: WafWindow = "24h") {
  return useQuery({
    queryKey: queryKeys.waf.statusFor(window),
    queryFn: () => wafApi.getWafStatus(window),
    refetchInterval: 15_000,
    // Switching window keeps the previous window's numbers on screen while the
    // new ones load, rather than collapsing the summary to a skeleton each time.
    placeholderData: (prev) => prev,
  });
}

/** Detection events, newest first, a page at a time. */
export function useWafEvents(filters: WafEventFilters) {
  return useInfiniteQuery({
    queryKey: queryKeys.waf.eventsFiltered(
      filters.clientIp ?? "",
      filters.ruleId ?? null,
    ),
    queryFn: ({ pageParam }) => wafApi.listWafEvents(filters, pageParam),
    initialPageParam: null as number | null,
    getNextPageParam: (last) => last.next_before,
    refetchInterval: 15_000,
  });
}

export function useSetWafEnabled() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: wafApi.setWafEnabled,
    onSuccess: (status) => {
      // The response is the state the proxy is in after the switch was applied,
      // but its summary only ever covers the default window — write it into that
      // window's entry alone, or a 7d view would relabel 24h numbers as 7d. The
      // invalidation below then refreshes whichever window is actually on screen.
      qc.setQueryData(queryKeys.waf.statusFor(status.summary.window), status);
      qc.invalidateQueries({ queryKey: queryKeys.waf.status });
      qc.invalidateQueries({ queryKey: queryKeys.proxyReconciler });
      qc.invalidateQueries({ queryKey: queryKeys.waf.events });
    },
  });
}
