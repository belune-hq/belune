import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { getCAPIStatus, changeCAPI } from "@/lib/api/capi";
import { queryKeys } from "./query-keys";

export function useCAPIStatus() {
  return useQuery({
    queryKey: queryKeys.crowdsec.capi,
    queryFn: getCAPIStatus,
    refetchInterval: (query) => (query.state.data?.busy ? 2_000 : 15_000),
  });
}
export function useChangeCAPI() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: changeCAPI,
    onSuccess: () =>
      client.invalidateQueries({ queryKey: queryKeys.crowdsec.capi }),
  });
}
