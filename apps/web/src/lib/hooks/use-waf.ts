import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { queryKeys } from "./query-keys";
import * as wafApi from "@/lib/api/waf";
import type { WafEventFilters } from "@/lib/api/waf";

export function useWafStatus() {
  return useQuery({
    queryKey: queryKeys.waf.status,
    queryFn: wafApi.getWafStatus,
    refetchInterval: 15_000,
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
      // The response is the state the proxy is in after the switch was applied.
      qc.setQueryData(queryKeys.waf.status, status);
      qc.invalidateQueries({ queryKey: queryKeys.proxyReconciler });
      qc.invalidateQueries({ queryKey: queryKeys.waf.events });
    },
  });
}
