import { useQuery } from "@tanstack/react-query";
import { queryKeys } from "./query-keys";
import * as proxyApi from "@/lib/api/proxy";

/**
 * Which optional route features the running Caddy can serve.
 *
 * Minutes of staleness is fine: this changes only when the operator swaps the
 * Caddy image. A failed request leaves `data` undefined, which callers must read
 * as "unknown" — never as "unavailable" — so a hiccup cannot make the UI claim
 * the operator's Caddy lacks a module.
 */
export function useProxyCapabilities() {
  return useQuery({
    queryKey: queryKeys.proxyCapabilities,
    queryFn: proxyApi.getProxyCapabilities,
    staleTime: 60_000,
  });
}
