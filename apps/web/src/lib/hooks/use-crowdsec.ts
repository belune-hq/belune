import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { queryKeys } from "./query-keys";
import * as crowdsecApi from "@/lib/api/crowdsec";
import type { CrowdSecStatus } from "@/lib/types";

export function useCrowdSecStatus() {
  return useQuery({
    queryKey: queryKeys.crowdsec.status,
    queryFn: crowdsecApi.getCrowdSecStatus,
    refetchInterval: 15_000,
  });
}

/** `enabled` is for a caller that knows the local API is not there to ask. */
export function useCrowdSecDecisions(enabled = true) {
  return useQuery({
    queryKey: queryKeys.crowdsec.decisions,
    queryFn: crowdsecApi.listCrowdSecDecisions,
    refetchInterval: 15_000,
    enabled,
  });
}

export function useCrowdSecAllowlist(enabled = true) {
  return useQuery({
    queryKey: queryKeys.crowdsec.allowlist,
    queryFn: crowdsecApi.listCrowdSecAllowlist,
    refetchInterval: 30_000,
    enabled,
  });
}

/** The statuses these return are the state AFTER the change was applied. */
function useStatusMutation<V>(fn: (v: V) => Promise<CrowdSecStatus>) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onSuccess: (status) => {
      qc.setQueryData(queryKeys.crowdsec.status, status);
      qc.invalidateQueries({ queryKey: queryKeys.proxyReconciler });
    },
  });
}

export function useSetCrowdSecEnabled() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: crowdsecApi.setCrowdSecEnabled,
    onSuccess: (status) => {
      qc.setQueryData(queryKeys.crowdsec.status, status);
      qc.invalidateQueries({ queryKey: queryKeys.proxyReconciler });
      // Turning it on allowlists the admin's address, which can lift a ban.
      qc.invalidateQueries({ queryKey: queryKeys.crowdsec.decisions });
      qc.invalidateQueries({ queryKey: queryKeys.crowdsec.allowlist });
    },
  });
}

export function useSetCrowdSecAutoban() {
  return useStatusMutation(crowdsecApi.setCrowdSecAutoban);
}

export function useRotateCrowdSecKey() {
  return useStatusMutation<void>(() => crowdsecApi.rotateCrowdSecKey());
}

export function useBanAddress() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: crowdsecApi.banAddress,
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: queryKeys.crowdsec.decisions }),
  });
}

export function useLiftBan() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: crowdsecApi.liftBan,
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: queryKeys.crowdsec.decisions }),
  });
}

export function useAllowAddress() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: crowdsecApi.allowAddress,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.crowdsec.allowlist });
      // Allowlisting an address lifts a ban already on it.
      qc.invalidateQueries({ queryKey: queryKeys.crowdsec.decisions });
    },
  });
}

export function useRemoveAllowed() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: crowdsecApi.removeAllowed,
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: queryKeys.crowdsec.allowlist }),
  });
}
