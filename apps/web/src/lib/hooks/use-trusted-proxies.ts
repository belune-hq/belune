import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { queryKeys } from "./query-keys";
import * as trustedProxiesApi from "@/lib/api/trusted-proxies";

export function useTrustedProxies() {
  return useQuery({
    queryKey: queryKeys.trustedProxies,
    queryFn: trustedProxiesApi.getTrustedProxies,
  });
}

export function useSetTrustedProxies() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: trustedProxiesApi.setTrustedProxies,
    onSuccess: (saved) => {
      // The response is the stored list with its overlaps, so write it rather than
      // refetch: the warning must be on screen the moment the list is.
      qc.setQueryData(queryKeys.trustedProxies, saved);
      qc.invalidateQueries({ queryKey: queryKeys.proxyReconciler });
    },
  });
}
