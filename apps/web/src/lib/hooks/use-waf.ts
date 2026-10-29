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

/**
 * Detection events, newest first, a page at a time.
 *
 * An expanded client's panel calls this with its own address, and is mounted
 * only while it is open — a table of fifty clients that each held this hook
 * would be fifty requests on first paint, refetching every 15 seconds.
 */
export function useWafEvents(filters: WafEventFilters) {
  return useInfiniteQuery({
    queryKey: queryKeys.waf.eventsFiltered(
      filters.clientIp ?? "",
      filters.ruleId ?? null,
      filters.window ?? "",
    ),
    queryFn: ({ pageParam }) => wafApi.listWafEvents(filters, pageParam),
    initialPageParam: null as number | null,
    getNextPageParam: (last) => last.next_before,
    refetchInterval: 15_000,
  });
}

/** The window's detections grouped by client. */
export function useWafClients(window: WafWindow, filters: WafEventFilters) {
  return useQuery({
    queryKey: queryKeys.waf.clientsFor(
      window,
      filters.clientIp ?? "",
      filters.ruleId ?? null,
    ),
    queryFn: () => wafApi.listWafClients(window, filters),
    refetchInterval: 15_000,
    // Changing window or filter keeps the table on screen while the next answer
    // loads, rather than collapsing it to a skeleton on every click.
    placeholderData: (prev) => prev,
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
      qc.invalidateQueries({ queryKey: queryKeys.waf.clients });
    },
  });
}

export function useWafExclusions() {
  return useQuery({
    queryKey: queryKeys.waf.exclusions,
    queryFn: wafApi.listWafExclusions,
  });
}

/**
 * An exclusion changes what the WAF reports from here on, so it refreshes the
 * status (whose exclusion count is the visible reason detections went quiet) and
 * the proxy's own state, and the detection lists.
 */
function useExclusionChange<TArg, TResult>(
  fn: (arg: TArg) => Promise<TResult>,
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.waf.exclusions });
      qc.invalidateQueries({ queryKey: queryKeys.waf.status });
      qc.invalidateQueries({ queryKey: queryKeys.proxyReconciler });
    },
  });
}

export function useCreateWafExclusion() {
  return useExclusionChange(wafApi.createWafExclusion);
}

export function useDeleteWafExclusion() {
  return useExclusionChange(wafApi.deleteWafExclusion);
}

export function useWafBlocking() {
  return useQuery({
    queryKey: queryKeys.waf.blocking,
    queryFn: wafApi.listWafBlocking,
  });
}

/**
 * Blocking changes what a 403 means, so it refreshes the status (the mode and
 * the count of enforced hosts), the proxy's own state, and the detection lists,
 * whose Blocked and Would-block badges read from the same switch.
 */
export function useSetWafBlocking() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: wafApi.setWafBlocking,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.waf.blocking });
      qc.invalidateQueries({ queryKey: queryKeys.waf.status });
      qc.invalidateQueries({ queryKey: queryKeys.proxyReconciler });
      qc.invalidateQueries({ queryKey: queryKeys.waf.events });
      qc.invalidateQueries({ queryKey: queryKeys.waf.clients });
    },
  });
}
